package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	Logger            *slog.Logger
	Timeout           float64
	TimeoutOffset     float64
	PrometheusTimeout float64
	UseCache          bool
	UseExpiredCache   bool
}

func (c HostFactCollector) Describe(_ chan<- *prometheus.Desc) {}

func (c HostFactCollector) Collect(ch chan<- prometheus.Metric) {
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
			cached, err := c.RingConfig.jsonClient.Get(ctx, hostsFactsKey)
			if err != nil {
				c.Logger.Error(fmt.Sprintf("Failed to get '%s' key from kvStore", hostsFactsKey), "err", err)
			}

			if cached != nil {
				cc := cached.(*Cache)

				if time.Now().After(time.Unix(0, cc.ExpiresAt)) {
					c.Logger.Debug(fmt.Sprintf("cache key '%s' time expired", hostsFactsKey))
					expired = true
				} else {
					found = true
				}

				content := cc.Content
				// zstd decompress data
				if c.CacheConfig.Compression {
					decoder, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
					decoded, err := decoder.DecodeAll(content, make([]byte, 0, len(content)))
					if err != nil {
						c.Logger.Error(fmt.Sprintf("Failed to decompress key '%s' value from kvStore", hostsFactsKey), "err", err)
						hostFactCollectorScrapeError(ch, 1.0)
						return
					}
					content = decoded
				}

				err = json.Unmarshal(content, &data)
				if err != nil {
					c.Logger.Error(fmt.Sprintf("Failed to decode key '%s' value from kvStore", hostsFactsKey), "err", err)
					hostFactCollectorScrapeError(ch, 1.0)
					return
				}
			}
		}
	} else {

		if c.CacheConfig.Enabled && c.UseCache {
			// Try to get the value from the local cache
			cached, ok := localCache.Get(hostsFactsKey)
			if ok {
				if time.Now().After(cached.ExpiresAt) {
					c.Logger.Debug(fmt.Sprintf("cache key '%s' time expired", hostsFactsKey))
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
				c.Logger.Error("Failed to get hosts facts filtered", "err", hostsFactsError)
			} else {
				for host, facts := range hostsFacts {
					labels := map[string]string{"name": host}
					for factName, factValue := range facts {

						replacer := strings.NewReplacer("/", "_", "-", "_", "::", "_", ".", "_")
						factNameSanitized := replacer.Replace(factName)
						if !labelNameRegexp.MatchString(factNameSanitized) {
							c.Logger.Error(fmt.Sprintf("Invalid Label Name %s. Must match the regex %s", factNameSanitized, labelNameRegexp))
							continue
						}
						labels[factNameSanitized] = factValue
					}
					hostsData = append(hostsData, labels)
				}

				// Add to the cache
				if c.RingConfig.enabled && c.CacheConfig.Enabled {
					content, _ := json.Marshal(hostsData)
					if c.CacheConfig.Compression {
						// use zstd to compress data
						encoder, _ := zstd.NewWriter(nil)
						content = encoder.EncodeAll(content, make([]byte, 0, len(content)))
					}
					if hostsFactsError == nil {
						// update the cache
						c.Logger.Info(fmt.Sprintf("updating cache key '%s'", hostsFactsKey))
						c.updateKV(content)
					}
				} else if c.CacheConfig.Enabled {
					// update the local cache
					c.Logger.Info(fmt.Sprintf("updating cache key '%s'", hostsFactsKey))
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
			c.Logger.Warn(fmt.Sprintf("scrape timeout %fs reached", timeout))
		}
	}

	// return expired cache on scrape error/timeout only if the param expired-cache is true
	if errVal == 1 || scrapeTimeoutVal == 1 {
		if c.CacheConfig.Enabled && c.UseExpiredCache {
			if len(data) != 0 {
				expiredCacheVal = 1
				c.Logger.Warn("use expired cache")
			} else {
				c.Logger.Warn("cache is empty")
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

func (c HostFactCollector) updateKV(content []byte) {
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
		Key:   hostsFactsKey,
		Value: val,
		Codec: cacheCodec.CodecID(),
	}

	msgBytes, _ := msg.Marshal()
	c.RingConfig.kvStore.NotifyMsg(msgBytes)
}
