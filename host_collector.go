package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/grafana/dskit/kv/memberlist"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fgouteroux/foreman_exporter/foreman"
)

var (
	hostsKey          = "collectors/host"
	hostCollectorLock = make(chan struct{}, 1)

	hostScrapeDurationMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "foreman_exporter_host_scrape_duration_seconds",
		Help: "Duration of the last completed host collector scrape of foreman.",
	})
)

type HostCollector struct {
	Client                *foreman.HTTPClient
	CacheConfig           *cacheConfig
	RingConfig            ExporterRing
	Logger                *slog.Logger
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
			c.Logger.Warn("Failed to determine ring leader", "err", err)
			return
		}
		if !isLeaderNow {
			c.Logger.Debug("skipping metrics collection as this node is not the ring leader")
			return
		}
		c.Logger.Debug("processing metrics collection as this node is the ring leader")

		if c.CacheConfig.Enabled {
			ctx := context.Background()
			cached, err := c.RingConfig.jsonClient.Get(ctx, hostsKey)
			if err != nil {
				c.Logger.Error(fmt.Sprintf("Failed to get '%s' key from kvStore", hostsKey), "err", err)
			}

			if cached != nil {

				if time.Now().After(time.Unix(0, cached.(*Cache).ExpiresAt)) {
					c.Logger.Debug(fmt.Sprintf("cache key '%s' time expired", hostsKey))
					expired = true
				} else {
					found = true
				}

				content := cached.(*Cache).Content
				// zstd decompress data
				if c.CacheConfig.Compression {
					decoded, err := zstdDecoder.DecodeAll(content, make([]byte, 0, len(content)))
					if err != nil {
						c.Logger.Error(fmt.Sprintf("Failed to decompress key '%s' value from kvStore", hostsKey), "err", err)
						hostCollectorScrapeError(ch, 1.0)
						return
					}
					content = decoded
				}

				err = json.Unmarshal(content, &data)
				if err != nil {
					c.Logger.Error(fmt.Sprintf("Failed to decode key '%s' value from kvStore", hostsKey), "err", err)
					hostCollectorScrapeError(ch, 1.0)
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
					c.Logger.Debug(fmt.Sprintf("cache key '%s' time expired", hostsKey))
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
	// servedFromScrape tells a fresh (possibly partial) result apart from a
	// value that came out of the cache.
	var servedFromScrape bool

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
			start := time.Now()

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
			// A short or partly failed host list is still usable: the client
			// returns the hosts it could collect alongside the error, and dropping
			// them left the exporter with no series at all.
			hostStatus, hostStatusError := c.Client.GetHostsFiltered(100)
			if hostStatusError != nil {
				c.Logger.Error("Failed to get hosts status filtered", "err", hostStatusError)
				errVal = 1
			}

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

				hostsData = append(hostsData, labelsFiltered)
			}

			// Measured on every outcome, so a slow-and-failing scrape is visible.
			elapsed := time.Since(start)
			hostScrapeDurationMetric.Set(elapsed.Seconds())

			// Add to the cache, but never from an incomplete list: it would
			// silently drop hosts for a whole TTL.
			if c.CacheConfig.Enabled && hostStatusError == nil && len(hostsData) > 0 {
				c.Logger.Info(fmt.Sprintf("updating cache key '%s'", hostsKey), "duration", elapsed.String(), "hosts", len(hostsData))
				if c.RingConfig.enabled {
					content, _ := json.Marshal(hostsData)
					if c.CacheConfig.Compression {
						// use zstd to compress data
						content = zstdEncoder.EncodeAll(content, make([]byte, 0, len(content)))
					}
					c.updateKV(content)
				} else {
					localCache.Set(hostsKey, hostsData, c.CacheConfig.ExpiresTTL)
				}
			}

			// return the data
			result <- hostsData
		}()

		// data currently holds the cached value, which may be expired. Keep it
		// aside so an empty scrape result cannot overwrite it with nothing.
		cached := data

		// using a select to return metrics under some conditions
		select {
		// task finished before the timeout
		case scraped := <-result:
			if len(scraped) > 0 {
				data = scraped
				servedFromScrape = true
			} else {
				data = cached
			}
		// another task is already running, no need to wait for the timeout
		case <-inflight:
		// task execution exceed the timeout, task will continue to running
		case <-time.After(deadline):
			scrapeTimeoutVal = 1
			c.Logger.Warn(fmt.Sprintf("scrape timeout %fs reached", timeout))
		}
	}

	// The scrape brought back nothing usable: fall back to the cached value,
	// which may be expired, and only when the caller opted in. A scrape that
	// succeeded for some hosts is served as-is, expired-cache flag untouched.
	if !servedFromScrape && (errVal == 1 || scrapeTimeoutVal == 1) {
		if c.CacheConfig.Enabled && c.UseExpiredCache && len(data) != 0 {
			expiredCacheVal = 1
			c.Logger.Warn("use expired cache")
		} else {
			if len(data) == 0 {
				c.Logger.Warn("cache is empty")
			}
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

func (c HostCollector) updateKV(content []byte) {
	now := time.Now()
	cache := &Cache{
		Content:   content,
		CreatedAt: now.UnixNano(),
		ExpiresAt: now.Add(c.CacheConfig.ExpiresTTL).UnixNano(),
	}

	val, err := cacheCodec.Encode(cache)
	if err != nil {
		c.Logger.Error(fmt.Sprintf("failed to encode data with '%s'", cacheCodec.CodecID()), "err", err)
		return
	}

	msg := memberlist.KeyValuePair{
		Key:   hostsKey,
		Value: val,
		Codec: cacheCodec.CodecID(),
	}

	msgBytes, _ := msg.Marshal()
	c.RingConfig.kvStore.NotifyMsg(msgBytes)
}
