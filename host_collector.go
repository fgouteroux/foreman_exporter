package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/kv/memberlist"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/klauspost/compress/zstd"

	"github.com/fgouteroux/foreman_exporter/foreman"
)

var (
	hostsKey          = "collectors/host"
	hostCollectorLock = make(chan struct{}, 1)
)

type HostCollector struct {
	Client                *foreman.HTTPClient
	CacheConfig           *cacheConfig
	RingConfig            ExporterRing
	Logger                log.Logger
	IncludeHostLabelRegex *regexp.Regexp
	ExcludeHostLabelRegex *regexp.Regexp
	Timeout               float64
	TimeoutOffset         float64
	PrometheusTimeout     float64
	UseCache              bool
	UseExpiredCache       bool
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
	var found bool
	var expired bool
	var data []map[string]string
	if c.RingConfig.enabled && c.UseCache {
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

		if c.CacheConfig.Enabled {
			ctx := context.Background()
			cached, err := c.RingConfig.jsonClient.Get(ctx, hostsKey)
			if err != nil {
				level.Error(c.Logger).Log("msg", fmt.Sprintf("Failed to get '%s' key from kvStore", hostsKey), "err", err) // #nosec G104
			}

			if cached != nil {

				if time.Now().After(cached.(*Cache).ExpiresAt) {
					level.Debug(c.Logger).Log("msg", fmt.Sprintf("cache key '%s' time expired", hostsKey)) // #nosec G104
					expired = true
				} else {
					found = true
				}

				content := cached.(*Cache).Content
				// zstd decompress data
				if c.CacheConfig.Compression {
					contentUnquoted, _ := strconv.Unquote(content)
					decoder, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
					decoded, err := decoder.DecodeAll([]byte(contentUnquoted), make([]byte, 0, len(contentUnquoted)))
					if err != nil {
						level.Error(c.Logger).Log("msg", fmt.Sprintf("Failed to decompress key '%s' value from kvStore", hostsKey), "err", err) // #nosec G104
						hostFactCollectorScrapeError(ch, 1.0)
						return
					}
					content = string(decoded)
				}

				err = json.Unmarshal([]byte(content), &data)
				if err != nil {
					level.Error(c.Logger).Log("msg", fmt.Sprintf("Failed to decode key '%s' value from kvStore", hostsKey), "err", err) // #nosec G104
					hostFactCollectorScrapeError(ch, 1.0)
					return
				}
			}
		}
	} else {

		if c.CacheConfig.Enabled && c.UseCache {
			// Try to get the value from the local cache
			cached, ok := localCache.Get(hostsKey)
			if ok {
				if time.Now().After(cached.ExpiresAt) {
					level.Debug(c.Logger).Log("msg", fmt.Sprintf("cache key '%s' time expired", hostsKey)) // #nosec G104
					expired = true
				} else {
					found = true
				}

				data = cached.Value.([]map[string]string)
			}
		}
	}

	var errVal float64
	var expiredCacheVal float64
	var scrapeTimeoutVal float64

	if !found || expired {
		var timeout float64
		if c.PrometheusTimeout != 0 {
			timeout = c.PrometheusTimeout - c.TimeoutOffset
		} else {
			timeout = c.Timeout - c.TimeoutOffset
		}

		deadline := time.Duration(timeout * float64(time.Second))

		result := make(chan []map[string]string, 1)
		inflight := make(chan bool, 1)

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

			var hostsData []map[string]string
			hostStatus, hostStatusError := c.Client.GetHostsFiltered(100)
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

					hostsData = append(hostsData, labels)
				}

				// Add to the cache
				if c.RingConfig.enabled && c.CacheConfig.Enabled {
					content, _ := json.Marshal(hostsData)
					if c.CacheConfig.Compression {
						// use zstd to compress data
						encoder, _ := zstd.NewWriter(nil)
						encoded := encoder.EncodeAll([]byte(content), make([]byte, 0, len(content)))
						content = []byte(strconv.Quote(string(encoded)))
					}
					if hostStatusError == nil {
						// update the cache
						level.Info(c.Logger).Log("msg", fmt.Sprintf("updating cache key '%s'", hostsKey)) // #nosec G104
						c.updateKV(string(content))
					}
				} else if c.CacheConfig.Enabled {
					// update the local cache
					level.Info(c.Logger).Log("msg", fmt.Sprintf("updating cache key '%s'", hostsKey)) // #nosec G104
					localCache.Set(hostsKey, hostsData, c.CacheConfig.ExpiresTTL)
				}
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
	}

	// return expired cache on scrape error/timeout only if the param expired-cache is true
	if errVal == 1 || scrapeTimeoutVal == 1 {
		if c.CacheConfig.Enabled && c.UseExpiredCache {
			if len(data) != 0 {
				expiredCacheVal = 1
				level.Warn(c.Logger).Log("msg", "use expired cache") // #nosec G104
			} else {
				level.Warn(c.Logger).Log("msg", "cache is empty") // #nosec G104
			}
		} else {
			data = nil
		}
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

	if c.CacheConfig.Enabled {
		hostCollectorExpiredCache(ch, expiredCacheVal)
	}
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

func hostCollectorExpiredCache(ch chan<- prometheus.Metric, val float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_use_expired_cache",
			"1 if using expired cache, 0 otherwise",
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

func (c HostCollector) updateKV(content string) {
	cache := &Cache{
		Content:   content,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(c.CacheConfig.ExpiresTTL),
	}

	val, err := JSONCodec.Encode(cache)
	if err != nil {
		level.Error(c.Logger).Log("msg", fmt.Sprintf("failed to encode data with '%s'", JSONCodec.CodecID()), "err", err) // #nosec G104
		return
	}

	msg := memberlist.KeyValuePair{
		Key:   hostsKey,
		Value: val,
		Codec: JSONCodec.CodecID(),
	}

	msgBytes, _ := msg.Marshal()
	c.RingConfig.kvStore.NotifyMsg(msgBytes)
}
