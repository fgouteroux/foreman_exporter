package main

import (
	"context"
	"net/http"
	"os"
	"regexp"
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

	concurrency = kingpin.Flag("concurrency", "Max concurrent http request").Default("4").Int64()
	limit       = kingpin.Flag("limit", "Foreman host limit search").Default("0").Int64()

	collectorsEnabled               = kingpin.Flag("collector", "collector to enabled (repeatable), choices: [host, hostfact]").Default("host").Enums("host", "hostfact")
	collectorHostLabelsIncludeRegex = kingpin.Flag("collector.host.labels-include", "host labels to include (regex)").Regexp()
	collectorHostLabelsExcludeRegex = kingpin.Flag("collector.host.labels-exclude", "host labels to exclude (regex)").Regexp()

	collectorHostFactSearch       = kingpin.Flag("collector.hostfact.search", "search host fact query filter").String()
	collectorHostFactIncludeRegex = kingpin.Flag("collector.hostfact.include", "host fact to include (regex)").Regexp()
	collectorHostFactExcludeRegex = kingpin.Flag("collector.hostfact.exclude", "host fact to exclude (regex)").Regexp()

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

func main() {

	log := logrus.New()
	log.Formatter = &logrus.JSONFormatter{}

	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("foreman-exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

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

	// Regist http handler
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
	} else if *cacheEnabled {
		localCache = memcache.NewLocalCache()
	}

	cacheCfg := &cacheConfig{
		Enabled:     *cacheEnabled,
		Compression: *cacheCompressionEnabled,
		ExpiresTTL:  time.Duration(cacheExpiresTTL.Seconds()) * time.Second,
	}

	http.Handle("/", indexHandler("", indexPage))

	hostFactRegistry := prometheus.NewRegistry()

	client := foreman.NewHTTPClient(
		*baseURL,
		*username,
		*password,
		*skipTLSVerify,
		*concurrency,
		*limit,
		*collectorHostFactSearch,
		*collectorHostFactIncludeRegex,
		*collectorHostFactExcludeRegex,
		nil,
		hostFactRegistry,
	)

	if slices.Contains(*collectorsEnabled, "host") {
		prometheus.MustRegister(HostCollector{
			Client:                client,
			Logger:                logger,
			RingConfig:            ringConfig,
			IncludeHostLabelRegex: *collectorHostLabelsIncludeRegex,
			ExcludeHostLabelRegex: *collectorHostLabelsExcludeRegex,
		})
	}
	if slices.Contains(*collectorsEnabled, "hostfact") {

		if *collectorHostFactSearch == "" && *collectorHostFactIncludeRegex == nil && *collectorHostFactExcludeRegex == nil {
			level.Warn(logger).Log("msg", "flags '--collector.hostfact.search' and '--collector.hostfact.include' and '--collector.hostfact.exclude' are not defined, it could cause big metrics labels !!") // #nosec G104
		}

		indexPage.AddLinks(defaultWeight, "Hosts Facts Metrics", []IndexPageLink{
			{Desc: "Exported Host Facts metrics", Path: "/hosts-facts-metrics"},
		})

		hostFactRegistry.MustRegister(HostFactCollector{
			Client:      client,
			Logger:      logger,
			RingConfig:  ringConfig,
			CacheConfig: cacheCfg,
		})
		http.Handle("/hosts-facts-metrics", promhttp.HandlerFor(hostFactRegistry, promhttp.HandlerOpts{}))
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
