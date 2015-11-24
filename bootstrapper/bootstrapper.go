package bootstrapper

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudfoundry-incubator/cf-mysql-bootstrap/clock"
	"github.com/cloudfoundry-incubator/cf-mysql-bootstrap/config"
	"github.com/pivotal-golang/lager"
)

const PollingIntervalInSec = 5

type Bootstrapper struct {
	rootConfig *config.Config
	clock      clock.Clock
}

func New(rootConfig *config.Config, clock clock.Clock) *Bootstrapper {
	return &Bootstrapper{
		rootConfig: rootConfig,
		clock:      clock,
	}
}

func (b *Bootstrapper) sendRequest(endpoint string, action string) (string, error) {
	resp, err := http.Get(endpoint)
	responseBody := ""
	if err != nil {
		return responseBody, fmt.Errorf("Failed to %s: %s", action, err.Error())
	}

	if resp.Body != nil {
		responseBytes, _ := ioutil.ReadAll(resp.Body)
		responseBody = string(responseBytes)
	}

	if resp.StatusCode != http.StatusOK {
		return responseBody, fmt.Errorf("Non 200 response from %s at %s: %s", action, endpoint, responseBody)
	}

	b.rootConfig.Logger.Info(fmt.Sprintf("Successfully sent %s request to URL: %s", action, endpoint))

	return responseBody, nil
}

func (b *Bootstrapper) pollUntilResponse(endpoint string, expectedResponse string) error {
	maxIterations := int(math.Ceil(float64(b.rootConfig.DatabaseStartupTimeout) / float64(PollingIntervalInSec)))
	sawResponse := false
	for i := 0; i < maxIterations; i++ {
		responseBody, err := b.sendRequest(endpoint, "mysql status")
		b.rootConfig.Logger.Info("Received response from status endpoint", lager.Data{
			"endpoint":     endpoint,
			"responseBody": responseBody,
		})
		if err != nil {
			continue //keep checking for valid response until timeout
		}

		if responseBody == expectedResponse {
			sawResponse = true
			break
		}
		<-b.clock.After(time.Duration(PollingIntervalInSec) * time.Second)
	}
	if sawResponse == false {
		return fmt.Errorf("Timed out waiting for %s from mysql after %d seconds", expectedResponse, b.rootConfig.DatabaseStartupTimeout)
	} else {
		b.rootConfig.Logger.Info(fmt.Sprintf("Successfully received %s response from mysql", expectedResponse), lager.Data{"url": endpoint})
		return nil
	}
}

func (b *Bootstrapper) isClusterHealthy() error {
	allNodes := len(b.rootConfig.HealthcheckURLs)
	syncedNodes := 0

	for _, url := range b.rootConfig.HealthcheckURLs {
		responseBody, err := b.sendRequest(url, "healthcheck")
		b.rootConfig.Logger.Info("Received response from node", lager.Data{
			"url":          url,
			"responseBody": responseBody,
		})
		if err == nil {
			syncedNodes++
		}
	}

	if syncedNodes == allNodes {
		err := errors.New("All nodes are synced. Bootstrap not required.")
		b.rootConfig.Logger.Error("Bootstrap not required", err)
		return err
	}

	if syncedNodes > 0 && syncedNodes != allNodes {
		err := errors.New("Cluster healthy but one or more nodes are failing. Bootstrap not required.")
		b.rootConfig.Logger.Error("Bootstrap not required", err)
		return err
	}

	return nil
}

func (b *Bootstrapper) waitForClusterShutdown() error {
	shutdownClusters := make(chan error, len(b.rootConfig.HealthcheckURLs))

	for _, url := range b.rootConfig.HealthcheckURLs {
		statusUrl := fmt.Sprintf("%s/%s", url, b.rootConfig.MysqlStatus)
		go func() {
			err := b.pollUntilResponse(statusUrl, "stopped")
			shutdownClusters <- err
		}()
	}

	for _ = range b.rootConfig.HealthcheckURLs {
		err := <-shutdownClusters
		if err != nil {
			return err
		}
	}

	b.rootConfig.Logger.Info("Successfully stopped mysql process on all vms")
	return nil
}

func (b *Bootstrapper) Run() error {
	logger := b.rootConfig.Logger

	err := b.isClusterHealthy()
	if err != nil {
		return err
	}

	for _, url := range b.rootConfig.HealthcheckURLs {
		statusMysqlUrl := fmt.Sprintf("%s/%s", url, b.rootConfig.MysqlStatus)
		_, err := b.sendRequest(statusMysqlUrl, "mysql status")
		if err != nil {
			return err
		}
	}

	for _, url := range b.rootConfig.HealthcheckURLs {
		stopMysqlUrl := fmt.Sprintf("%s/%s", url, b.rootConfig.ShutDownMysql)
		_, err := b.sendRequest(stopMysqlUrl, "stop mysql")
		if err != nil {
			return err
		}
	}

	err = b.waitForClusterShutdown()
	if err != nil {
		return err
	}

	sequenceNumberMap := make(map[string]int)
	for _, url := range b.rootConfig.HealthcheckURLs {
		getSeqNumberUrl := fmt.Sprintf("%s/%s", url, b.rootConfig.GetSeqNumber)
		responseBody, err := b.sendRequest(getSeqNumberUrl, "get sequence number")
		if err != nil {
			return err
		}

		sequenceNumber, err := strconv.Atoi(responseBody)
		if err != nil {
			return fmt.Errorf("Failed to get valid sequence number from %s with %s", getSeqNumberUrl, err.Error())
		}

		logger.Info(fmt.Sprintf("Retrieved sequence number of %d from %s", sequenceNumber, getSeqNumberUrl), lager.Data{
			"url": getSeqNumberUrl,
		})

		sequenceNumberMap[url] = sequenceNumber
	}

	bootstrapNode, joinNodes := largestSequenceNumber(sequenceNumberMap)
	bootstrapReqURL := fmt.Sprintf("%s/%s", bootstrapNode, b.rootConfig.StartMysqlInBootstrapMode)
	_, err = b.sendRequest(bootstrapReqURL, "bootstrap mysql node")
	if err != nil {
		return err
	}

	statusUrl := fmt.Sprintf("%s/%s", bootstrapNode, b.rootConfig.MysqlStatus)

	err = b.pollUntilResponse(statusUrl, "running")
	if err != nil {
		return err
	}

	for _, joinNode := range joinNodes {
		joinReqURL := fmt.Sprintf("%s/%s", joinNode, b.rootConfig.StartMysqlInJoinMode)
		_, err := b.sendRequest(joinReqURL, "join mysql")

		if err != nil {
			return err
		}
	}

	for _, url := range joinNodes {
		statusUrl := fmt.Sprintf("%s/%s", url, b.rootConfig.MysqlStatus)
		err = b.pollUntilResponse(statusUrl, "running")
		if err != nil {
			return err
		}
	}

	logger.Info("Successfully started mysql process on all joining nodes")

	return nil
}

func largestSequenceNumber(seqMap map[string]int) (string, []string) {
	maxSeq := -1
	maxSeqURL := ""
	joinNodes := []string{}
	for url, seqno := range seqMap {
		if seqno > maxSeq {
			maxSeq = seqno
			maxSeqURL = url
		}
	}

	for url, _ := range seqMap {
		if url != maxSeqURL {
			joinNodes = append(joinNodes, url)
		}
	}

	return maxSeqURL, joinNodes
}
