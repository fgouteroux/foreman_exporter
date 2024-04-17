package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fgouteroux/foreman_exporter/foreman"
)

var hostCollectorLock = make(chan struct{}, 1)

type HostCollector struct {
	Client                *foreman.HTTPClient
	RingConfig            ExporterRing
	Logger                log.Logger
	IncludeHostLabelRegex *regexp.Regexp
	ExcludeHostLabelRegex *regexp.Regexp
	Timeout               float64
	TimeoutOffset         float64
	PrometheusTimeout     float64
}

type HostLabels struct {
	ID                       int64  `json:"id"`
	Name                     string `json:"name"`
	GlobalStatusLabel        string `json:"global_status,omitempty"`
	ConfigurationStatusLabel string `json:"configuration_status,omitempty"`
	BuildStatusLabel         string `json:"build_status,omitempty"`
	OrganizationName         string `json:"organization,omitempty"`
	EnvironmentName          string `json:"environment,omitempty"`
	OperatingSystemName      string `json:"operatingsystem,omitempty"`
	OwnerName                string `json:"owner,omitempty"`
	LocationName             string `json:"location,omitempty"`
	ModelName                string `json:"model,omitempty"`
	HostgroupName            string `json:"hostgroup,omitempty"`
}

func (c HostCollector) Describe(_ chan<- *prometheus.Desc) {}

func (c HostCollector) Collect(ch chan<- prometheus.Metric) {

	if c.RingConfig.enabled {
		// If another replica is the leader, don't expose any metrics from this one.
		isLeaderNow, err := isLeader(c.RingConfig)
		if err != nil {
			level.Warn(c.Logger).Log("msg", "Failed to determine ring leader", "err", err) // #nosec G104
			return
		}
		if !isLeaderNow {
			level.Debug(c.Logger).Log("msg", "skipping metrics collection as this node is not the ring leader") // #nosec G104
			return
		}
		level.Debug(c.Logger).Log("msg", "processing metrics collection as this node is the ring leader") // #nosec G104
	}

	var timeout float64
	if c.PrometheusTimeout != 0 {
		timeout = c.PrometheusTimeout - c.TimeoutOffset
	} else {
		timeout = c.Timeout - c.TimeoutOffset
	}

	deadline := time.Duration(timeout * float64(time.Second))

	result := make(chan []map[string]string, 1)
	inflight := make(chan bool, 1)

	var errVal float64
	var scrapeTimeoutVal float64
	var data []map[string]string
	// use a goroutine to get result in async mode
	go func() {

		if *collectorsLock {
			// lock and return directly if another request is in progress
			select {
			case hostCollectorLock <- struct{}{}:
				defer func() { <-hostCollectorLock }()
				hostCollectorInflightRequestBlocking(ch, 0)
			default:
				hostCollectorInflightRequestBlocking(ch, 1)
				// another request is running, notify the chann to not wait for the timeout
				inflight <- true
				return
			}
		}

		hostStatus, hostStatusError := c.Client.GetHostsFiltered(100)
		if hostStatusError != nil {
			level.Error(c.Logger).Log("msg", "Failed to get hosts status filtered", "err", hostStatusError) // #nosec G104
			errVal = 1
		}

		var hostsData []map[string]string
		for _, host := range hostStatus {
			var labels map[string]string
			data, _ := json.Marshal(HostLabels(host))
			_ = json.Unmarshal(data, &labels)
			delete(labels, "id")

			labelsFiltered := make(map[string]string, len(labels))
			for k, v := range labels {
				labelsFiltered[k] = v
			}

			for label := range labels {

				if label == "name" {
					continue
				}
				if c.IncludeHostLabelRegex != nil && len(c.IncludeHostLabelRegex.FindStringSubmatch(label)) == 0 {
					delete(labelsFiltered, label)
					continue
				}

				if c.ExcludeHostLabelRegex != nil && len(c.ExcludeHostLabelRegex.FindStringSubmatch(label)) > 0 {
					delete(labelsFiltered, label)
				}
			}

			hostsData = append(hostsData, labels)
		}

		// return the data
		result <- hostsData
	}()

	// using a select to return metrics under some conditions
	select {
	// task finished before the timeout
	case data = <-result:
		close(result)
	// another task is already running, no need to wait for the timeout
	case <-inflight:
		close(inflight)
	// task execution exceed the timeout, task will continue to running
	case <-time.After(deadline):
		scrapeTimeoutVal = 1
		level.Warn(c.Logger).Log("msg", fmt.Sprintf("scrape timeout %fs reached", timeout)) // #nosec G104
	}

	// return metrics
	for _, labels := range data {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				"foreman_exporter_host_status_info",
				"Foreman host status",
				nil, labels,
			),
			prometheus.GaugeValue, 1,
		)
	}

	hostCollectorScrapeError(ch, errVal)
	hostCollectorScrapeTimeout(ch, scrapeTimeoutVal)
}

func hostCollectorScrapeTimeout(ch chan<- prometheus.Metric, val float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_scrape_timeout",
			"1 if timeout occurs, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, val,
	)
}

func hostCollectorScrapeError(ch chan<- prometheus.Metric, errVal float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_scrape_error",
			"1 if there was an error, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, errVal,
	)
}

func hostCollectorInflightRequestBlocking(ch chan<- prometheus.Metric, val float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_inflight_blocking_request",
			"",
			nil, nil,
		),
		prometheus.GaugeValue, val,
	)
}
