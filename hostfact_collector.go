package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/kv/memberlist"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/klauspost/compress/zstd"

	"github.com/fgouteroux/foreman_exporter/foreman"
)

var (
	hostsFactsKey          = "collectors/host-fact"
	hostFactsCollectorLock = make(chan struct{}, 1)
)

type HostFactCollector struct {
	Client            *foreman.HTTPClient
	CacheConfig       *cacheConfig
	RingConfig        ExporterRing
	Logger            log.Logger
	Timeout           float64
	TimeoutOffset     float64
	PrometheusTimeout float64
	UseExpiredCache   bool
}

func (c HostFactCollector) Describe(_ chan<- *prometheus.Desc) {}

func (c HostFactCollector) Collect(ch chan<- prometheus.Metric) {
	var found bool
	var expired bool
	var data []map[string]string
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

		if c.CacheConfig.Enabled {
			ctx := context.Background()
			cached, err := c.RingConfig.jsonClient.Get(ctx, hostsFactsKey)
			if err != nil {
				level.Error(c.Logger).Log("msg", fmt.Sprintf("Failed to get '%s' key from kvStore", hostsFactsKey), "err", err) // #nosec G104
			}

			if cached != nil {

				if time.Now().After(cached.(*Cache).ExpiresAt) {
					level.Debug(c.Logger).Log("msg", fmt.Sprintf("cache key '%s' time expired", hostsFactsKey)) // #nosec G104
					expired = true
				} else {
					found = true
				}

				content := cached.(*Cache).Content
				// zstd decompress data
				if *cacheCompressionEnabled {
					contentUnquoted, _ := strconv.Unquote(content)
					decoder, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
					decoded, err := decoder.DecodeAll([]byte(contentUnquoted), make([]byte, 0, len(contentUnquoted)))
					if err != nil {
						level.Error(c.Logger).Log("msg", fmt.Sprintf("Failed to decompress key '%s' value from kvStore", hostsFactsKey), "err", err) // #nosec G104
						hostFactCollectorScrapeError(ch, 1.0)
						return
					}
					content = string(decoded)
				}

				err = json.Unmarshal([]byte(content), &data)
				if err != nil {
					level.Error(c.Logger).Log("msg", fmt.Sprintf("Failed to decode key '%s' value from kvStore", hostsFactsKey), "err", err) // #nosec G104
					hostFactCollectorScrapeError(ch, 1.0)
					return
				}
			}
		}
	} else {

		if c.CacheConfig.Enabled {
			// Try to get the value from the local cache
			cached, ok := localCache.Get(hostsFactsKey)
			if ok {
				if time.Now().After(cached.ExpiresAt) {
					level.Debug(c.Logger).Log("msg", fmt.Sprintf("cache key '%s' time expired", hostsFactsKey)) // #nosec G104
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

	if !found || expired || !c.UseExpiredCache {

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
				case hostFactsCollectorLock <- struct{}{}:
					defer func() { <-hostFactsCollectorLock }()
					hostFactCollectorInflightRequestBlocking(ch, 0)
				default:
					hostFactCollectorInflightRequestBlocking(ch, 1)
					// another request is running, notify the chann to not wait for the timeout
					inflight <- true
					return
				}
			}

			var hostsData []map[string]string
			hostsFacts, hostsFactsError := c.Client.GetHostsFactsFiltered(1000)
			if hostsFactsError != nil {
				errVal = 1
				level.Error(c.Logger).Log("msg", "Failed to get hosts facts filtered", "err", hostsFactsError) // #nosec G104
			} else {
				for host, facts := range hostsFacts {
					labels := map[string]string{"name": host}
					for factName, factValue := range facts {

						replacer := strings.NewReplacer("/", "_", "-", "_", "::", "_", ".", "_")
						factNameSanitized := replacer.Replace(factName)
						if !labelNameRegexp.MatchString(factNameSanitized) {
							level.Error(c.Logger).Log("msg", fmt.Sprintf("Invalid Label Name %s. Must match the regex %s", factNameSanitized, labelNameRegexp)) // #nosec G104
							continue
						}
						labels[factNameSanitized] = factValue
					}
					hostsData = append(hostsData, labels)
				}

				// Add to the cache
				if c.RingConfig.enabled && c.CacheConfig.Enabled {
					content, _ := json.Marshal(hostsData)
					if *cacheCompressionEnabled {
						// use zstd to compress data
						encoder, _ := zstd.NewWriter(nil)
						encoded := encoder.EncodeAll([]byte(content), make([]byte, 0, len(content)))
						content = []byte(strconv.Quote(string(encoded)))
					}
					if hostsFactsError == nil {
						// update the cache
						level.Info(c.Logger).Log("msg", fmt.Sprintf("updating cache key '%s'", hostsFactsKey)) // #nosec G104
						c.updateKV(string(content))
					}
				} else if c.CacheConfig.Enabled {
					// update the local cache
					level.Info(c.Logger).Log("msg", fmt.Sprintf("updating cache key '%s'", hostsFactsKey)) // #nosec G104
					localCache.Set(hostsFactsKey, hostsData, c.CacheConfig.ExpiresTTL)
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
		// task execution exceed the timeout, task will continue to running and to udpate the cache
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
				"foreman_exporter_host_facts_info",
				"Foreman host facts",
				nil, labels,
			),
			prometheus.GaugeValue, 1,
		)
	}

	hostFactCollectorScrapeError(ch, errVal)
	hostFactCollectorScrapeTimeout(ch, scrapeTimeoutVal)

	if c.CacheConfig.Enabled {
		hostFactCollectorExpiredCache(ch, expiredCacheVal)
	}
}

func hostFactCollectorScrapeTimeout(ch chan<- prometheus.Metric, val float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_facts_scrape_timeout",
			"1 if timeout occurs, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, val,
	)
}

func hostFactCollectorExpiredCache(ch chan<- prometheus.Metric, val float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_facts_use_expired_cache",
			"1 if using expired cache, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, val,
	)
}

func hostFactCollectorScrapeError(ch chan<- prometheus.Metric, errVal float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_facts_scrape_error",
			"1 if there was an error, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, errVal,
	)
}

func hostFactCollectorInflightRequestBlocking(ch chan<- prometheus.Metric, val float64) {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"foreman_exporter_host_facts_inflight_blocking_request",
			"",
			nil, nil,
		),
		prometheus.GaugeValue, val,
	)
}

func (c HostFactCollector) updateKV(content string) {
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
		Key:   hostsFactsKey,
		Value: val,
		Codec: JSONCodec.CodecID(),
	}

	msgBytes, _ := msg.Marshal()
	c.RingConfig.kvStore.NotifyMsg(msgBytes)
}
