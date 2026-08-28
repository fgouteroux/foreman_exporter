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

	"github.com/fgouteroux/foreman_exporter/foreman"
)

var (
	hostsFactsKey          = "collectors/host-fact"
	hostFactsCollectorLock = make(chan struct{}, 1)

	hostFactScrapeDurationMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "foreman_exporter_host_facts_scrape_duration_seconds",
		Help: "Duration of the last completed host facts collector scrape of foreman.",
	})

	// factNameReplacer turns foreman fact names into valid label names. It is
	// stateless and safe for concurrent use, so it is built once rather than per
	// fact (this used to be allocated in the innermost loop, i.e. hosts x facts
	// times on every scrape).
	factNameReplacer = strings.NewReplacer("/", "_", "-", "_", "::", "_", ".", "_")
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
	// CacheOnPartial allows a scrape that failed for some hosts to still refresh
	// the cache. Partial results are always exported either way.
	CacheOnPartial bool
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
					decoded, err := zstdDecoder.DecodeAll(content, make([]byte, 0, len(content)))
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
			// A partial result is still a result: the client returns the hosts it
			// could collect alongside the error, and throwing that away meant a
			// single rate-limited host wasted the whole (multi-minute) scrape and
			// left the exporter with no series at all.
			hostsFacts, hostsFactsError := c.Client.GetHostsFactsFiltered()
			if hostsFactsError != nil {
				errVal = 1
				c.Logger.Error("Failed to get hosts facts filtered", "err", hostsFactsError)
			}

			for host, facts := range hostsFacts {
				labels := map[string]string{"name": host}
				for factName, factValue := range facts {
					factNameSanitized := factNameReplacer.Replace(factName)
					if !labelNameRegexp.MatchString(factNameSanitized) {
						c.Logger.Error(fmt.Sprintf("Invalid Label Name %s. Must match the regex %s", factNameSanitized, labelNameRegexp))
						continue
					}
					labels[factNameSanitized] = factValue
				}
				hostsData = append(hostsData, labels)
			}

			// Measured on every outcome, so a slow-and-failing scrape is visible.
			elapsed := time.Since(start)
			hostFactScrapeDurationMetric.Set(elapsed.Seconds())

			// Add to the cache. A partial scrape only refreshes it when asked to,
			// so the cache is not silently degraded by a bad run.
			if c.CacheConfig.Enabled && len(hostsData) > 0 && (hostsFactsError == nil || c.CacheOnPartial) {
				c.Logger.Info(fmt.Sprintf("updating cache key '%s'", hostsFactsKey), "duration", elapsed.String(), "hosts", len(hostsData), "partial", hostsFactsError != nil)
				if c.RingConfig.enabled {
					content, _ := json.Marshal(hostsData)
					if c.CacheConfig.Compression {
						// use zstd to compress data
						content = zstdEncoder.EncodeAll(content, make([]byte, 0, len(content)))
					}
					c.updateKV(content)
				} else {
					localCache.Set(hostsFactsKey, hostsData, c.CacheConfig.ExpiresTTL)
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
		// task execution exceed the timeout, task will continue to running and to udpate the cache
		case <-time.After(deadline):
			scrapeTimeoutVal = 1
			c.Logger.Warn(fmt.Sprintf("scrape timeout %fs reached", timeout))
		}
	}

	// The scrape brought back nothing usable: fall back to the cached value,
	// which may be expired, and only when the caller opted in. A scrape that
	// succeeded for some hosts is served as-is, expired-cache flag untouched.
	if !servedFromScrape && (errVal == 1 || scrapeTimeoutVal == 1) {
		switch {
		case c.CacheConfig.Enabled && c.UseExpiredCache && len(data) != 0:
			expiredCacheVal = 1
			c.Logger.Warn("use expired cache")
		case len(data) != 0:
			// There is a cached value but the caller did not ask for it.
			data = nil
		case !c.CacheConfig.Enabled:
			c.Logger.Warn("no data to export: the scrape failed and the cache is disabled")
		case !c.UseCache:
			// The cache was never read, so nothing can be said about it: with
			// ?cache=false the caller asked for a live scrape and it failed.
			c.Logger.Warn("no data to export: the scrape failed and the cache was bypassed by the request")
		default:
			c.Logger.Warn("no data to export: the scrape failed and the cache is empty")
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
