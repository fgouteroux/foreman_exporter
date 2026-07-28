package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	promslogflag "github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"github.com/sirupsen/logrus"

	"github.com/fgouteroux/foreman_exporter/foreman"
	"github.com/fgouteroux/foreman_exporter/memcache"
)

var (
	disableExporterMetrics = kingpin.Flag("web.disable-exporter-metrics", "Exclude metrics about the exporter itself (process_*, go_*).").Bool()
	metricsPath            = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
	prefixPath             = kingpin.Flag("web.prefix-path", "Prefix path for all http requests.").Default("").String()
	webConfig              = webflag.AddFlags(kingpin.CommandLine, ":11111")

	baseURL       = kingpin.Flag("url", "Foreman url.").Envar("FOREMAN_URL").Required().URL()
	username      = kingpin.Flag("username", "Foreman username.").Envar("FOREMAN_USERNAME").Required().String()
	password      = kingpin.Flag("password", "Foreman password").Envar("FOREMAN_PASSWORD").Required().String()
	skipTLSVerify = kingpin.Flag("skip-tls-verify", "Foreman skip TLS verify.").Envar("FOREMAN_SKIP_TLS_VERIFY").Bool()

	concurrency   = kingpin.Flag("concurrency", "Max concurrent foreman client http request.").Default("4").Int64()
	retryMax      = kingpin.Flag("retry-max", "Max retries for foreman client http requests (honors the Retry-After header on rate-limit responses).").Default("3").Int64()
	limit         = kingpin.Flag("limit", "Foreman client host limit search.").Default("0").Int64()
	search        = kingpin.Flag("search", "Foreman client host search filter.").Default("").String()
	timeoutOffset = kingpin.Flag("timeout-offset", "Offset to subtract from Prometheus-supplied timeout.").Default("0.5s").Duration()

	// Lock concurrent requests on collectors to avoid flooding foreman api with too many requests
	collectorsLock = kingpin.Flag("collector.lock-concurrent-requests", "Lock concurrent requests on collectors.").Bool()

	collectorsEnabled = kingpin.Flag("collector", "Collector to enabled (repeatable), choices: [host, hostfact].").Default("host").Enums("host", "hostfact")

	collectorHostLabelsIncludeRegex      = kingpin.Flag("collector.host.labels-include", "Host labels to include (regex).").Regexp()
	collectorHostLabelsExcludeRegex      = kingpin.Flag("collector.host.labels-exclude", "Host labels to exclude (regex).").Regexp()
	collectorHostTimeout                 = kingpin.Flag("collector.host.timeout", "Host default timeout if no request header 'X-Prometheus-Scrape-Timeout-Seconds'").Default("30s").Duration()
	collectorHostCacheEnabled            = kingpin.Flag("collector.host.cache.enabled", "Enable host cache, if global 'cache.enabled' is false.").Bool()
	collectorHostCacheCompressionEnabled = kingpin.Flag("collector.host.cache.compression", "Enable host zstd cache compression for kvstore values, if global 'cache.compression' is false.").Bool()
	collectorHostCacheExpiresTTL         = kingpin.Flag("collector.host.cache.ttl-expires", "Host cache expiration time, if omitted, inherit from 'cache.ttl-expires'.").Duration()

	collectorHostFactSearch                  = kingpin.Flag("collector.hostfact.search", "Search host fact query filter.").String()
	collectorHostFactIncludeRegex            = kingpin.Flag("collector.hostfact.include", "Host fact to include (regex).").Regexp()
	collectorHostFactExcludeRegex            = kingpin.Flag("collector.hostfact.exclude", "Host fact to exclude (regex).").Regexp()
	collectorHostFactTimeout                 = kingpin.Flag("collector.hostfact.timeout", "Host fact default timeout if no request header 'X-Prometheus-Scrape-Timeout-Seconds'.").Default("30s").Duration()
	collectorHostFactCacheEnabled            = kingpin.Flag("collector.hostfact.cache.enabled", "Enable host fact cache, if global 'cache.enabled' is false.").Bool()
	collectorHostFactCacheCompressionEnabled = kingpin.Flag("collector.hostfact.cache.compression", "Enable host fact zstd cache compression for kvstore values, if global 'cache.compression' is false.").Bool()
	collectorHostFactCacheExpiresTTL         = kingpin.Flag("collector.hostfact.cache.ttl-expires", "Host fact cache expiration time, if omitted, inherit from global 'cache.ttl-expires'.").Duration()

	cacheEnabled            = kingpin.Flag("cache.enabled", "Enable cache for all collectors.").Bool()
	cacheExpiresTTL         = kingpin.Flag("cache.ttl-expires", "Cache Expiration time for all collectors.").Default("1h").Duration()
	cacheCompressionEnabled = kingpin.Flag("cache.compression", "Enable zstd cache compression for all collectors in kvstore.").Bool()

	ringEnabled                = kingpin.Flag("ring.enabled", "Enable the ring to deduplicate exported foreman metrics.").Bool()
	ringInstanceID             = kingpin.Flag("ring.instance-id", "Instance ID to register in the ring.").String()
	ringInstanceAddr           = kingpin.Flag("ring.instance-addr", "IP address to advertise in the ring. Default is auto-detected.").String()
	ringInstancePort           = kingpin.Flag("ring.instance-port", "Port to advertise in the ring.").Default("7946").Int()
	ringInstanceInterfaceNames = kingpin.Flag("ring.instance-interface-names", "List of network interface names to look up when finding the instance IP address.").String()
	ringJoinMembers            = kingpin.Flag("ring.join-members", "Other cluster members to join.").String()

	localCache *memcache.MemCache

	labelNameRegexp = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

type cacheConfig struct {
	Enabled     bool
	Compression bool
	ExpiresTTL  time.Duration
}

func formatFilePath(path string) string {
	arr := strings.Split(path, "/")
	return arr[len(arr)-1]
}

type UTCFormatter struct {
	logrus.Formatter
}

func (u UTCFormatter) Format(e *logrus.Entry) ([]byte, error) {
	e.Time = e.Time.UTC()
	return u.Formatter.Format(e)
}

func main() {

	log := logrus.New()
	log.SetReportCaller(true)
	log.SetFormatter(UTCFormatter{&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z",
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime: "ts",
			logrus.FieldKeyFile: "caller",
		},
		CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			return "", fmt.Sprintf("%s:%d", formatFilePath(f.File), f.Line)
		},
	}})

	promslogConfig := &promslog.Config{}
	promslogflag.AddFlags(kingpin.CommandLine, promslogConfig)
	kingpin.Version(version.Print("foreman-exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	lvl, _ := logrus.ParseLevel(promslogConfig.Level.String())
	log.SetLevel(lvl)

	logger := promslog.New(promslogConfig)

	if *disableExporterMetrics {
		prometheus.Unregister(collectors.NewGoCollector())
		prometheus.Unregister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}

	err := prometheus.Register(versioncollector.NewCollector("foreman_exporter"))
	if err != nil {
		logger.Error("Error registering version collector", "err", err)
	}

	logger.Info("Starting foreman-exporter", "version", version.Info())
	logger.Info("Build context", "build_context", version.BuildContext())

	http.Handle(*metricsPath, promhttp.Handler())
	http.Handle("/static/", http.FileServer(http.FS(staticFiles)))

	indexPage := newIndexPageContent()
	indexPage.AddLinks(metricsWeight, "Metrics", []IndexPageLink{
		{Desc: "Exported metrics", Path: "/metrics"},
	})
	indexPage.AddLinks(defaultWeight, "Status", []IndexPageLink{
		{Desc: "Status (JSON)", Path: "/status"},
	})
	var ringConfig ExporterRing
	if *ringEnabled {
		ctx := context.Background()
		ringConfig, err = newRing(*ringInstanceID, *ringInstanceAddr, *ringJoinMembers, *ringInstanceInterfaceNames, *ringInstancePort, newGoKitLogger(logger))
		defer services.StopAndAwaitTerminated(ctx, ringConfig.memberlistsvc) //nolint:errcheck
		defer services.StopAndAwaitTerminated(ctx, ringConfig.lifecycler)    //nolint:errcheck
		defer services.StopAndAwaitTerminated(ctx, ringConfig.client)        //nolint:errcheck

		if err != nil {
			logger.Error("failed to initialize ring", "err", err)
			os.Exit(1)
		}

		indexPage.AddLinks(ringWeight, "Ring", []IndexPageLink{
			{Desc: "Ring status", Path: "/ring"},
		})
		indexPage.AddLinks(memberlistWeight, "Memberlist", []IndexPageLink{
			{Desc: "Membership status", Path: "/memberlist"},
		})

		http.Handle("/ring", ringConfig.lifecycler)
		http.Handle("/memberlist", memberlistStatusHandler("", ringConfig.memberlistsvc))
	} else if *collectorHostFactCacheEnabled || *collectorHostCacheEnabled {
		localCache = memcache.NewLocalCache()
	}

	var collectorsInfo []collectorInfo

	client := foreman.NewHTTPClient(
		*baseURL,
		*username,
		*password,
		*skipTLSVerify,
		*concurrency,
		*limit,
		*retryMax,
		*search,
		*collectorHostFactSearch,
		*collectorHostFactIncludeRegex,
		*collectorHostFactExcludeRegex,
		log,
	)

	if slices.Contains(*collectorsEnabled, "hostfact") {

		logger.Info("collector host fact enabled")

		if *collectorHostFactSearch == "" && *collectorHostFactIncludeRegex == nil && *collectorHostFactExcludeRegex == nil {
			logger.Warn("flags '--collector.hostfact.search' and '--collector.hostfact.include' and '--collector.hostfact.exclude' are not defined, it could cause big metrics labels !!")
		}

		indexPage.AddLinks(hostFactWeight, "Host facts", []IndexPageLink{
			{Desc: "Scrape host facts", Path: "/host-facts-metrics"},
		})

		var collectorCacheEnabled bool
		var collectorCacheCompression bool
		var collectorCacheExpiresTTL time.Duration
		if *cacheEnabled {
			collectorCacheEnabled = true
		} else {
			collectorCacheEnabled = *collectorHostFactCacheEnabled
		}

		if *cacheCompressionEnabled {
			collectorCacheCompression = true
		} else {
			collectorCacheCompression = *collectorHostFactCacheCompressionEnabled
		}

		if collectorHostFactCacheExpiresTTL.Seconds() == 0 {
			collectorCacheExpiresTTL = *cacheExpiresTTL
		} else {
			collectorCacheExpiresTTL = *collectorHostFactCacheExpiresTTL
		}

		logger.Info("collector host fact cache", "enabled", collectorCacheEnabled, "ttl", collectorCacheExpiresTTL, "compression", cacheCompressionEnabled)

		cacheCfg := &cacheConfig{
			Enabled:     collectorCacheEnabled,
			Compression: collectorCacheCompression,
			ExpiresTTL:  time.Duration(collectorCacheExpiresTTL.Seconds()) * time.Second,
		}

		collectorsInfo = append(collectorsInfo, collectorInfo{
			Name:         "hostfact",
			CacheEnabled: collectorCacheEnabled,
			TTL:          collectorCacheExpiresTTL.String(),
			Compression:  collectorCacheCompression,
		})

		collector := HostFactCollector{
			Client:        client,
			Logger:        logger,
			RingConfig:    ringConfig,
			CacheConfig:   cacheCfg,
			TimeoutOffset: timeoutOffset.Seconds(),
			Timeout:       collectorHostFactTimeout.Seconds(),
			UseCache:      true,
		}

		http.HandleFunc("/host-facts-metrics", func(w http.ResponseWriter, req *http.Request) {
			hostFactHandler(w, req, collector)
		})
	}

	if slices.Contains(*collectorsEnabled, "host") {

		logger.Info("collector host enabled")

		indexPage.AddLinks(hostWeight, "Host", []IndexPageLink{
			{Desc: "Scrape host metrics", Path: "/host-metrics"},
		})

		var collectorCacheEnabled bool
		var collectorCacheCompression bool
		var collectorCacheExpiresTTL time.Duration
		if *cacheEnabled {
			collectorCacheEnabled = true
		} else {
			collectorCacheEnabled = *collectorHostCacheEnabled
		}

		if *cacheCompressionEnabled {
			collectorCacheCompression = true
		} else {
			collectorCacheCompression = *collectorHostCacheCompressionEnabled
		}

		if collectorHostCacheExpiresTTL.Seconds() == 0 {
			collectorCacheExpiresTTL = *cacheExpiresTTL
		} else {
			collectorCacheExpiresTTL = *collectorHostCacheExpiresTTL
		}

		logger.Info("collector host cache", "enabled", collectorCacheEnabled, "ttl", collectorCacheExpiresTTL, "compression", cacheCompressionEnabled)

		cacheCfg := &cacheConfig{
			Enabled:     collectorCacheEnabled,
			Compression: collectorCacheCompression,
			ExpiresTTL:  time.Duration(collectorCacheExpiresTTL.Seconds()) * time.Second,
		}

		collectorsInfo = append(collectorsInfo, collectorInfo{
			Name:         "host",
			CacheEnabled: collectorCacheEnabled,
			TTL:          collectorCacheExpiresTTL.String(),
			Compression:  collectorCacheCompression,
		})

		collector := HostCollector{
			Client:                client,
			Logger:                logger,
			RingConfig:            ringConfig,
			CacheConfig:           cacheCfg,
			IncludeHostLabelRegex: *collectorHostLabelsIncludeRegex,
			ExcludeHostLabelRegex: *collectorHostLabelsExcludeRegex,
			TimeoutOffset:         timeoutOffset.Seconds(),
			Timeout:               collectorHostTimeout.Seconds(),
			UseCache:              true,
		}

		http.HandleFunc("/host-metrics", func(w http.ResponseWriter, req *http.Request) {
			hostHandler(w, req, collector)
		})
	}

	static := pageStatic{
		Version:    version.Version,
		Revision:   version.Revision,
		ForemanURL: (*baseURL).Redacted(),
		Collectors: collectorsInfo,
	}
	http.Handle("/", indexHandler("", indexPage, static, ringConfig))
	http.Handle("/status", statusHandler(static, ringConfig))

	server := &http.Server{
		ReadTimeout:       120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := web.ListenAndServe(server, webConfig, logger); err != nil {
		logger.Error("failed to start web server", "err", err)
		os.Exit(1)
	}
}
