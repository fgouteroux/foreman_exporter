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

var hostsFactsKey = "collectors/host-fact"

type HostFactCollector struct {
	Client      *foreman.HTTPClient
	CacheConfig *cacheConfig
	RingConfig  ExporterRing
	Logger      log.Logger
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
			level.Debug(c.Logger).Log("msg", "not the ring leader") // #nosec G104
			return
		}
		level.Debug(c.Logger).Log("msg", "ring leader") // #nosec G104

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
	} else {
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

	var errVal float64

	if !found || expired {
		hostsFacts, hostsFactsError := c.Client.GetHostsFactsFiltered(1000)
		if hostsFactsError != nil {
			level.Error(c.Logger).Log("msg", "Failed to get hosts facts filtered", "err", hostsFactsError) // #nosec G104
			errVal = 1
			if expired {
				level.Warn(c.Logger).Log("msg", "Use expired cache") // #nosec G104
			}
		} else {
			// clear data
			data = nil
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
				data = append(data, labels)
			}

			// Add to the cache
			if c.RingConfig.enabled {
				content, _ := json.Marshal(data)
				if *cacheCompressionEnabled {
					// use zstd to compress data
					encoder, _ := zstd.NewWriter(nil)
					encoded := encoder.EncodeAll([]byte(content), make([]byte, 0, len(content)))
					content = []byte(strconv.Quote(string(encoded)))
				}
				if hostsFactsError == nil {
					// update the cache
					c.updateKV(string(content))
				}
			} else {
				localCache.Set(hostsFactsKey, data, c.CacheConfig.ExpiresTTL)
			}
		}
	}

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
