package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"github.com/sirupsen/logrus"

	"golang.org/x/exp/slices"

	"github.com/fgouteroux/foreman_exporter/foreman"
	"github.com/fgouteroux/foreman_exporter/memcache"
)

var (
	disableExporterMetrics = kingpin.Flag("web.disable-exporter-metrics", "Exclude metrics about the exporter itself (process_*, go_*).").Bool()
	metricsPath            = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
	prefixPath             = kingpin.Flag("web.prefix-path", "Prefix path for all http requests.").Default("").String()
	webConfig              = webflag.AddFlags(kingpin.CommandLine, ":11111")

	baseURL       = kingpin.Flag("url", "Foreman url").Envar("FOREMAN_URL").Required().URL()
	username      = kingpin.Flag("username", "Foreman username").Envar("FOREMAN_USERNAME").Required().String()
	password      = kingpin.Flag("password", "Foreman password").Envar("FOREMAN_PASSWORD").Required().String()
	skipTLSVerify = kingpin.Flag("skip-tls-verify", "Foreman skip TLS verify").Envar("FOREMAN_SKIP_TLS_VERIFY").Bool()

	concurrency   = kingpin.Flag("concurrency", "Max concurrent http request").Default("4").Int64()
	limit         = kingpin.Flag("limit", "Foreman host limit search").Default("0").Int64()
	search        = kingpin.Flag("search", "Foreman host search filter").Default("").String()
	timeoutOffset = kingpin.Flag("timeout-offset", "Offset to subtract from Prometheus-supplied timeout.").Default("0.5s").Duration()

	// Lock concurrent requests on collectors to avoid flooding foreman api with too many requests
	collectorsLock = kingpin.Flag("collector.lock-concurrent-requests", "lock concurrent requests on collectors").Bool()

	collectorsEnabled = kingpin.Flag("collector", "collector to enabled (repeatable), choices: [host, hostfact]").Default("host").Enums("host", "hostfact")

	collectorHostLabelsIncludeRegex      = kingpin.Flag("collector.host.labels-include", "host labels to include (regex)").Regexp()
	collectorHostLabelsExcludeRegex      = kingpin.Flag("collector.host.labels-exclude", "host labels to exclude (regex)").Regexp()
	collectorHostTimeout                 = kingpin.Flag("collector.host.timeout", "host timeout").Default("30s").Duration()
	collectorHostCacheEnabled            = kingpin.Flag("collector.host.cache.enabled", "enable host cache").Bool()
	collectorHostCacheCompressionEnabled = kingpin.Flag("collector.host.cache.compression", "enable host cache zstd compression for kvstore values").Bool()
	collectorHostCacheExpiresTTL         = kingpin.Flag("collector.host.cache.ttl-expires", "host cache expiration time").Duration()

	collectorHostFactSearch                  = kingpin.Flag("collector.hostfact.search", "search host fact query filter").String()
	collectorHostFactIncludeRegex            = kingpin.Flag("collector.hostfact.include", "host fact to include (regex)").Regexp()
	collectorHostFactExcludeRegex            = kingpin.Flag("collector.hostfact.exclude", "host fact to exclude (regex)").Regexp()
	collectorHostFactTimeout                 = kingpin.Flag("collector.hostfact.timeout", "host fact timeout").Default("30s").Duration()
	collectorHostFactCacheEnabled            = kingpin.Flag("collector.hostfact.cache.enabled", "enable host fact cache").Bool()
	collectorHostFactCacheCompressionEnabled = kingpin.Flag("collector.hostfact.cache.compression", "enable host fact cache zstd compression for kvstore values").Bool()
	collectorHostFactCacheExpiresTTL         = kingpin.Flag("collector.hostfact.cache.ttl-expires", "host fact cache expiration time").Duration()

	cacheEnabled            = kingpin.Flag("cache.enabled", "Enable cache").Bool()
	cacheExpiresTTL         = kingpin.Flag("cache.ttl-expires", "Cache Expiration time").Default("1h").Duration()
	cacheCompressionEnabled = kingpin.Flag("cache.compression", "Enable zstd compression for kvstore values").Bool()

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

	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("foreman-exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	lvl, _ := logrus.ParseLevel(promlogConfig.Level.String())
	log.SetLevel(lvl)

	logger := promlog.New(promlogConfig)

	if *disableExporterMetrics {
		prometheus.Unregister(collectors.NewGoCollector())
		prometheus.Unregister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}

	err := prometheus.Register(version.NewCollector("foreman_exporter"))
	if err != nil {
		level.Error(logger).Log("msg", "Error registering version collector", "err", err) // #nosec G104
	}

	level.Info(logger).Log("msg", "Starting foreman-exporter", "version", version.Info())   // #nosec G104
	level.Info(logger).Log("msg", "Build context", "build_context", version.BuildContext()) // #nosec G104

	http.Handle(*metricsPath, promhttp.Handler())
	http.Handle("/static/", http.FileServer(http.FS(staticFiles)))

	indexPage := newIndexPageContent()
	indexPage.AddLinks(metricsWeight, "Metrics", []IndexPageLink{
		{Desc: "Exported metrics", Path: "/metrics"},
	})
	var ringConfig ExporterRing
	if *ringEnabled {
		ctx := context.Background()
		ringConfig, err = newRing(*ringInstanceID, *ringInstanceAddr, *ringJoinMembers, *ringInstanceInterfaceNames, *ringInstancePort, logger)
		defer services.StopAndAwaitTerminated(ctx, ringConfig.memberlistsvc) //nolint:errcheck
		defer services.StopAndAwaitTerminated(ctx, ringConfig.lifecycler)    //nolint:errcheck
		defer services.StopAndAwaitTerminated(ctx, ringConfig.client)        //nolint:errcheck

		if err != nil {
			level.Error(logger).Log("err", err) // #nosec G104
			os.Exit(1)
		}

		indexPage.AddLinks(ringWeight, "Exporter", []IndexPageLink{
			{Desc: "Ring status", Path: "/ring"},
		})
		indexPage.AddLinks(memberlistWeight, "Memberlist", []IndexPageLink{
			{Desc: "Status", Path: "/memberlist"},
		})

		http.Handle("/ring", ringConfig.lifecycler)
		http.Handle("/memberlist", memberlistStatusHandler("", ringConfig.memberlistsvc))
	} else if *collectorHostFactCacheEnabled || *collectorHostCacheEnabled {
		localCache = memcache.NewLocalCache()
	}

	http.Handle("/", indexHandler("", indexPage))

	client := foreman.NewHTTPClient(
		*baseURL,
		*username,
		*password,
		*skipTLSVerify,
		*concurrency,
		*limit,
		*search,
		*collectorHostFactSearch,
		*collectorHostFactIncludeRegex,
		*collectorHostFactExcludeRegex,
		log,
	)

	if slices.Contains(*collectorsEnabled, "hostfact") {

		level.Info(logger).Log("msg", "collector host fact enabled") // #nosec G104

		if *collectorHostFactSearch == "" && *collectorHostFactIncludeRegex == nil && *collectorHostFactExcludeRegex == nil {
			level.Warn(logger).Log("msg", "flags '--collector.hostfact.search' and '--collector.hostfact.include' and '--collector.hostfact.exclude' are not defined, it could cause big metrics labels !!") // #nosec G104
		}

		indexPage.AddLinks(hostFactWeight, "Hosts Facts Metrics", []IndexPageLink{
			{Desc: "Exported Host Facts metrics", Path: "/host-facts-metrics"},
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

		level.Info(logger).Log("msg", "collector host fact cache", "enabled", collectorCacheEnabled, "ttl", collectorCacheExpiresTTL, "compression", cacheCompressionEnabled) // #nosec G104

		cacheCfg := &cacheConfig{
			Enabled:     collectorCacheEnabled,
			Compression: collectorCacheCompression,
			ExpiresTTL:  time.Duration(collectorCacheExpiresTTL.Seconds()) * time.Second,
		}

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

		level.Info(logger).Log("msg", "collector host enabled") // #nosec G104

		indexPage.AddLinks(hostWeight, "Hosts Metrics", []IndexPageLink{
			{Desc: "Exported Host metrics", Path: "/host-metrics"},
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

		level.Info(logger).Log("msg", "collector host cache", "enabled", collectorCacheEnabled, "ttl", collectorCacheExpiresTTL, "compression", cacheCompressionEnabled) // #nosec G104

		cacheCfg := &cacheConfig{
			Enabled:     collectorCacheEnabled,
			Compression: collectorCacheCompression,
			ExpiresTTL:  time.Duration(collectorCacheExpiresTTL.Seconds()) * time.Second,
		}

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

	server := &http.Server{
		ReadTimeout:       120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := web.ListenAndServe(server, webConfig, logger); err != nil {
		level.Error(logger).Log("err", err) // #nosec G104
		os.Exit(1)
	}
}
