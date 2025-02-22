package edge

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/portainer/agent"
	"github.com/portainer/agent/chisel"
	"github.com/portainer/agent/edge/scheduler"
	"github.com/portainer/agent/edge/stack"
	"github.com/portainer/libcrypto"
)

const tunnelActivityCheckInterval = 30 * time.Second

// PollService is used to poll a Portainer instance to retrieve the status associated to the Edge endpoint.
// It is responsible for managing the state of the reverse tunnel (open and closing after inactivity).
// It is also responsible for retrieving the data associated to Edge stacks and schedules.
type PollService struct {
	apiServerAddr           string
	pollIntervalInSeconds   float64
	pollTicker              *time.Ticker
	insecurePoll            bool
	inactivityTimeout       time.Duration
	edgeID                  string
	httpClient              *http.Client
	tunnelClient            agent.ReverseTunnelClient
	scheduleManager         agent.Scheduler
	lastActivity            time.Time
	updateLastActivity      chan struct{}
	startSignal             chan struct{}
	stopSignal              chan struct{}
	edgeStackManager        *stack.StackManager
	portainerURL            string
	endpointID              string
	tunnelServerAddr        string
	tunnelServerFingerprint string
	logsManager             *scheduler.LogsManager
	containerPlatform       agent.ContainerPlatform
}

type pollServiceConfig struct {
	APIServerAddr           string
	EdgeID                  string
	InactivityTimeout       string
	PollFrequency           string
	InsecurePoll            bool
	TunnelCapability        bool
	PortainerURL            string
	EndpointID              string
	TunnelServerAddr        string
	TunnelServerFingerprint string
	ContainerPlatform       agent.ContainerPlatform
}

// newPollService returns a pointer to a new instance of PollService, and will start two loops in go routines.
// The first loop will poll the Portainer instance for the status of the associated endpoint and create a reverse tunnel
// if needed as well as manage schedules.
// The second loop will check for the last activity of the reverse tunnel and close the tunnel if it exceeds the tunnel
// inactivity duration.
// If TunneCapability is disabled, it will only poll for Edge stacks and schedule without managing reverse tunnels.
func newPollService(edgeStackManager *stack.StackManager, logsManager *scheduler.LogsManager, config *pollServiceConfig) (*PollService, error) {
	pollFrequency, err := time.ParseDuration(config.PollFrequency)
	if err != nil {
		return nil, err
	}

	inactivityTimeout, err := time.ParseDuration(config.InactivityTimeout)
	if err != nil {
		return nil, err
	}

	pollService := &PollService{
		apiServerAddr:           config.APIServerAddr,
		edgeID:                  config.EdgeID,
		pollIntervalInSeconds:   pollFrequency.Seconds(),
		pollTicker:              time.NewTicker(pollFrequency),
		insecurePoll:            config.InsecurePoll,
		inactivityTimeout:       inactivityTimeout,
		scheduleManager:         scheduler.NewCronManager(),
		updateLastActivity:      make(chan struct{}),
		startSignal:             make(chan struct{}),
		stopSignal:              make(chan struct{}),
		edgeStackManager:        edgeStackManager,
		portainerURL:            config.PortainerURL,
		endpointID:              config.EndpointID,
		tunnelServerAddr:        config.TunnelServerAddr,
		tunnelServerFingerprint: config.TunnelServerFingerprint,
		logsManager:             logsManager,
		containerPlatform:       config.ContainerPlatform,
	}

	if config.TunnelCapability {
		pollService.tunnelClient = chisel.NewClient()
	}

	go pollService.startStatusPollLoop()
	go pollService.startActivityMonitoringLoop()

	return pollService, nil
}

func (service *PollService) resetActivityTimer() {
	if service.tunnelClient != nil && service.tunnelClient.IsTunnelOpen() {
		service.updateLastActivity <- struct{}{}
	}
}

func (service *PollService) start() {
	service.startSignal <- struct{}{}
}

func (service *PollService) stop() {
	service.stopSignal <- struct{}{}
}

func (service *PollService) startStatusPollLoop() {
	var pollCh <-chan time.Time

	log.Printf("[DEBUG] [edge] [poll_interval_seconds: %f] [server_url: %s] [message: starting Portainer short-polling client]", service.pollIntervalInSeconds, service.portainerURL)

	for {
		select {
		case <-pollCh:
			err := service.poll()
			if err != nil {
				log.Printf("[ERROR] [edge] [message: an error occured during short poll] [error: %s]", err)
			}
		case <-service.startSignal:
			pollCh = service.pollTicker.C
		case <-service.stopSignal:
			log.Println("[DEBUG] [edge] [message: stopping Portainer short-polling client]")
			pollCh = nil
		}
	}
}

func (service *PollService) startActivityMonitoringLoop() {
	ticker := time.NewTicker(tunnelActivityCheckInterval)

	log.Printf("[DEBUG] [edge] [monitoring_interval_seconds: %f] [inactivity_timeout: %s] [message: starting activity monitoring loop]", tunnelActivityCheckInterval.Seconds(), service.inactivityTimeout.String())

	for {
		select {
		case <-ticker.C:
			if service.lastActivity.IsZero() {
				continue
			}

			elapsed := time.Since(service.lastActivity)
			log.Printf("[DEBUG] [edge] [tunnel_last_activity_seconds: %f] [message: tunnel activity monitoring]", elapsed.Seconds())

			if service.tunnelClient != nil && service.tunnelClient.IsTunnelOpen() && elapsed.Seconds() > service.inactivityTimeout.Seconds() {
				log.Printf("[INFO] [edge] [tunnel_last_activity_seconds: %f] [message: shutting down tunnel after inactivity period]", elapsed.Seconds())

				err := service.tunnelClient.CloseTunnel()
				if err != nil {
					log.Printf("[ERROR] [edge] [message: unable to shutdown tunnel] [error: %s]", err)
				}
			}
		case <-service.updateLastActivity:
			service.lastActivity = time.Now()
		}
	}
}

const clientDefaultPollTimeout = 5

type stackStatus struct {
	ID      int
	Version int
}

type pollStatusResponse struct {
	Status          string           `json:"status"`
	Port            int              `json:"port"`
	Schedules       []agent.Schedule `json:"schedules"`
	CheckinInterval float64          `json:"checkin"`
	Credentials     string           `json:"credentials"`
	Stacks          []stackStatus    `json:"stacks"`
}

func (service *PollService) createHTTPClient(timeout float64) {
	httpCli := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}

	if service.insecurePoll {
		httpCli.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}
	}

	service.httpClient = httpCli
}

func (service *PollService) poll() error {

	pollURL := fmt.Sprintf("%s/api/endpoints/%s/status", service.portainerURL, service.endpointID)
	req, err := http.NewRequest("GET", pollURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set(agent.HTTPEdgeIdentifierHeaderName, service.edgeID)

	// When the header is not set to PlatformDocker Portainer assumes the platform to be kubernetes.
	// However, Portainer should handle podman agents the same way as docker agents.
	agentPlatformIdentifier := service.containerPlatform
	if service.containerPlatform == agent.PlatformPodman {
		agentPlatformIdentifier = agent.PlatformDocker
	}
	req.Header.Set(agent.HTTPResponseAgentPlatform, strconv.Itoa(int(agentPlatformIdentifier)))

	log.Printf("[DEBUG] [edge] [message: sending agent platform header] [header: %s]", strconv.Itoa(int(agentPlatformIdentifier)))

	if service.httpClient == nil {
		service.createHTTPClient(clientDefaultPollTimeout)
	}

	resp, err := service.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[DEBUG] [edge] [response_code: %d] [message: Poll request failure]", resp.StatusCode)
		return errors.New("short poll request failed")
	}

	var responseData pollStatusResponse
	err = json.NewDecoder(resp.Body).Decode(&responseData)
	if err != nil {
		return err
	}

	log.Printf("[DEBUG] [edge] [status: %s] [port: %d] [schedule_count: %d] [checkin_interval_seconds: %f]", responseData.Status, responseData.Port, len(responseData.Schedules), responseData.CheckinInterval)

	if service.tunnelClient != nil {
		if responseData.Status == "IDLE" && service.tunnelClient.IsTunnelOpen() {
			log.Printf("[DEBUG] [edge] [status: %s] [message: Idle status detected, shutting down tunnel]", responseData.Status)

			err := service.tunnelClient.CloseTunnel()
			if err != nil {
				log.Printf("[ERROR] [edge] [message: Unable to shutdown tunnel] [error: %s]", err)
			}
		}

		if responseData.Status == "REQUIRED" && !service.tunnelClient.IsTunnelOpen() {
			log.Println("[DEBUG] [edge] [message: Required status detected, creating reverse tunnel]")

			err := service.createTunnel(responseData.Credentials, responseData.Port)
			if err != nil {
				log.Printf("[ERROR] [edge] [message: Unable to create tunnel] [error: %s]", err)
				return err
			}
		}
	}

	err = service.scheduleManager.Schedule(responseData.Schedules)
	if err != nil {
		log.Printf("[ERROR] [edge] [message: an error occurred during schedule management] [err: %s]", err)
	}

	logsToCollect := []int{}
	for _, schedule := range responseData.Schedules {
		if schedule.CollectLogs {
			logsToCollect = append(logsToCollect, schedule.ID)
		}
	}

	service.logsManager.HandleReceivedLogsRequests(logsToCollect)

	if responseData.CheckinInterval != service.pollIntervalInSeconds {
		log.Printf("[DEBUG] [edge] [old_interval: %f] [new_interval: %f] [message: updating poll interval]", service.pollIntervalInSeconds, responseData.CheckinInterval)
		service.pollIntervalInSeconds = responseData.CheckinInterval
		service.createHTTPClient(responseData.CheckinInterval)
		service.pollTicker.Reset(time.Duration(service.pollIntervalInSeconds) * time.Second)
	}

	if responseData.Stacks != nil {
		stacks := map[int]int{}
		for _, stack := range responseData.Stacks {
			stacks[stack.ID] = stack.Version
		}

		err := service.edgeStackManager.UpdateStacksStatus(stacks)
		if err != nil {
			log.Printf("[ERROR] [edge] [message: an error occurred during stack management] [error: %s]", err)
			return err
		}
	}

	return nil
}

func (service *PollService) createTunnel(encodedCredentials string, remotePort int) error {
	decodedCredentials, err := base64.RawStdEncoding.DecodeString(encodedCredentials)
	if err != nil {
		return err
	}

	credentials, err := libcrypto.Decrypt(decodedCredentials, []byte(service.edgeID))
	if err != nil {
		return err
	}

	tunnelConfig := agent.TunnelConfig{
		ServerAddr:       service.tunnelServerAddr,
		ServerFingerpint: service.tunnelServerFingerprint,
		Credentials:      string(credentials),
		RemotePort:       strconv.Itoa(remotePort),
		LocalAddr:        service.apiServerAddr,
	}

	err = service.tunnelClient.CreateTunnel(tunnelConfig)
	if err != nil {
		return err
	}

	service.resetActivityTimer()
	return nil
}
