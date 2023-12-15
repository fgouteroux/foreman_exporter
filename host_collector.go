package main

import (
	"encoding/json"
	"regexp"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fgouteroux/foreman_exporter/foreman"
)

type HostCollector struct {
	Client                *foreman.HTTPClient
	RingConfig            ExporterRing
	Logger                log.Logger
	IncludeHostLabelRegex *regexp.Regexp
	ExcludeHostLabelRegex *regexp.Regexp
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

	var errVal float64
	hostStatus, hostStatusError := c.Client.GetHostsFiltered("", 100)

	if hostStatusError != nil {
		level.Error(c.Logger).Log("msg", "Failed to get hosts status filtered", "err", hostStatusError) // #nosec G104
		errVal = 1
	} else {
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

			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					"foreman_exporter_host_status_info",
					"Foreman host status",
					nil, labelsFiltered,
				),
				prometheus.GaugeValue, 1,
			)
		}
	}

	hostCollectorScrapeError(ch, errVal)
}

func hostCollectorScrapeError(ch chan<- prometheus.Metric, errVal float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_status_scrape_error",
			"1 if there was an error, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, errVal,
	)
}
