package main

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/grafana/dskit/kv/memberlist"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed index.gohtml
var indexPageHTML string

func newIndexPageContent() *IndexPageContent {
	return &IndexPageContent{}
}

type indexPageContents struct {
	LinkGroups []IndexPageLinkGroup
	Version    string
	Revision   string
	ForemanURL string
	Collectors []collectorInfo
	Ring       ringStatus
}

// pageStatic holds the parts of the index page that don't change at runtime.
// It is captured once when the handlers are built.
type pageStatic struct {
	Version    string
	Revision   string
	ForemanURL string
	Collectors []collectorInfo
}

// collectorInfo summarises an enabled collector's cache configuration.
type collectorInfo struct {
	Name         string `json:"name"`
	CacheEnabled bool   `json:"cache_enabled"`
	TTL          string `json:"ttl,omitempty"`
	Compression  bool   `json:"compression"`
}

// ringStatus is the ring state shown on the index page. It is computed per
// request since membership and leadership can change at any time. The index page
// only shows the leader; the full member list is kept for the /status endpoint.
type ringStatus struct {
	Enabled   bool         `json:"enabled"`
	Err       string       `json:"error,omitempty"`
	Members   []ringMember `json:"members,omitempty"`
	Leader    ringMember   `json:"-"`
	HasLeader bool         `json:"-"`
}

type ringMember struct {
	ID     string `json:"id"`
	Addr   string `json:"addr"`
	State  string `json:"state"`
	Leader bool   `json:"leader"`
	Self   bool   `json:"self"`
	URL    string `json:"url,omitempty"`
}

// webURLForAddr builds a link to a member's own web UI. The ring address carries
// the gossip port, so we swap in the port (and scheme) the client used to reach
// this node, assuming every node exposes its UI on the same port.
func webURLForAddr(r *http.Request, ringAddr string) string {
	host, _, err := net.SplitHostPort(ringAddr)
	if err != nil {
		host = ringAddr
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "" {
		return scheme + "://" + net.JoinHostPort(host, port) + "/"
	}
	return scheme + "://" + host + "/"
}

// ringStatusForRequest resolves the ring status and fills each member's web-UI
// URL based on the incoming request (scheme/port).
func ringStatusForRequest(r *http.Request, ringCfg ExporterRing) ringStatus {
	rs := ringStatusFor(ringCfg)
	for i := range rs.Members {
		rs.Members[i].URL = webURLForAddr(r, rs.Members[i].Addr)
		if rs.Members[i].Leader {
			rs.Leader = rs.Members[i]
			rs.HasLeader = true
		}
	}
	return rs
}

// ringStatusFor resolves the current ring members and leader. When the ring is
// disabled it returns a zero value (Enabled=false); when the ring is up but the
// members can't be listed it returns the error for display.
func ringStatusFor(r ExporterRing) ringStatus {
	if !r.enabled {
		return ringStatus{}
	}

	leaderAddr := ""
	if rl, err := ringLeader(r.client); err == nil {
		leaderAddr = rl.Addr
	}

	rs, err := r.client.GetAllHealthy(ringOp)
	if err != nil {
		return ringStatus{Enabled: true, Err: err.Error()}
	}

	self := r.lifecycler.GetInstanceAddr()
	members := make([]ringMember, 0, len(rs.Instances))
	for _, inst := range rs.Instances {
		members = append(members, ringMember{
			ID:     inst.Id,
			Addr:   inst.Addr,
			State:  inst.State.String(),
			Leader: leaderAddr != "" && inst.Addr == leaderAddr,
			Self:   inst.Addr == self,
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	return ringStatus{Enabled: true, Members: members}
}

// IndexPageContent is a map of sections to path -> description.
type IndexPageContent struct {
	mu sync.Mutex

	elements []IndexPageLinkGroup
}

type IndexPageLinkGroup struct {
	weight int
	Desc   string
	Links  []IndexPageLink
}

type IndexPageLink struct {
	Desc string
	Path string
}

// List of weights to order link groups in the same order as weights are ordered here.
const (
	metricsWeight = iota
	hostWeight
	hostFactWeight
	defaultWeight
	ringWeight
	memberlistWeight
)

func (pc *IndexPageContent) AddLinks(weight int, groupDesc string, links []IndexPageLink) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.elements = append(pc.elements, IndexPageLinkGroup{weight: weight, Desc: groupDesc, Links: links})
}

func (pc *IndexPageContent) GetContent() []IndexPageLinkGroup {
	pc.mu.Lock()
	els := append([]IndexPageLinkGroup(nil), pc.elements...)
	pc.mu.Unlock()

	sort.Slice(els, func(i, j int) bool {
		if els[i].weight != els[j].weight {
			return els[i].weight < els[j].weight
		}
		return els[i].Desc < els[j].Desc
	})

	return els
}

//go:embed static
var staticFiles embed.FS

func indexHandler(httpPathPrefix string, content *IndexPageContent, static pageStatic, ringCfg ExporterRing) http.HandlerFunc {
	templ := template.New("main")
	templ.Funcs(map[string]interface{}{
		"AddPathPrefix": func(link string) string {
			return path.Join(httpPathPrefix, link)
		},
	})
	template.Must(templ.Parse(indexPageHTML))

	return func(w http.ResponseWriter, r *http.Request) {
		err := templ.Execute(w, indexPageContents{
			LinkGroups: content.GetContent(),
			Version:    static.Version,
			Revision:   static.Revision,
			ForemanURL: static.ForemanURL,
			Collectors: static.Collectors,
			Ring:       ringStatusForRequest(r, ringCfg),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// statusHandler exposes the same version/config/ring information as the index
// page, as JSON, for automation.
func statusHandler(static pageStatic, ringCfg ExporterRing) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Version    string          `json:"version"`
			Revision   string          `json:"revision"`
			ForemanURL string          `json:"foreman_url"`
			Collectors []collectorInfo `json:"collectors"`
			Ring       ringStatus      `json:"ring"`
		}{
			Version:    static.Version,
			Revision:   static.Revision,
			ForemanURL: static.ForemanURL,
			Collectors: static.Collectors,
			Ring:       ringStatusForRequest(r, ringCfg),
		})
	}
}

//go:embed memberlist_status.gohtml
var memberlistStatusPageHTML string

func memberlistStatusHandler(httpPathPrefix string, kvs *memberlist.KVInitService) http.Handler {
	templ := template.New("memberlist_status")
	templ.Funcs(map[string]interface{}{
		"AddPathPrefix": func(link string) string { return path.Join(httpPathPrefix, link) },
		"StringsJoin":   strings.Join,
	})
	template.Must(templ.Parse(memberlistStatusPageHTML))
	return memberlist.NewHTTPStatusHandler(kvs, templ)
}

func hostFactHandler(w http.ResponseWriter, r *http.Request, collector HostFactCollector) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)

	registry := prometheus.NewRegistry()

	collector.Client.SetHostsFactsRegistry(registry)

	// Get Prometheus timeout header
	collector.PrometheusTimeout, _ = strconv.ParseFloat(r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"), 64)

	expiredCacheParam := r.URL.Query().Get("expired-cache")
	if expiredCacheParam != "" {
		if !collector.CacheConfig.Enabled {
			http.Error(w, "cache not enabled", http.StatusBadRequest)
			return
		}

		var err error
		collector.UseExpiredCache, err = strconv.ParseBool(expiredCacheParam)
		if err != nil {
			http.Error(w, "expired-cache should be a boolean", http.StatusBadRequest)
			return
		}
	}

	cacheParam := r.URL.Query().Get("cache")
	if cacheParam != "" {
		if !collector.CacheConfig.Enabled {
			http.Error(w, "cache not enabled", http.StatusBadRequest)
			return
		}

		var err error
		collector.UseCache, err = strconv.ParseBool(cacheParam)
		if err != nil {
			http.Error(w, "cache should be a boolean", http.StatusBadRequest)
			return
		}
	}

	registry.MustRegister(collector)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func hostHandler(w http.ResponseWriter, r *http.Request, collector HostCollector) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)

	registry := prometheus.NewRegistry()
	collector.Client.SetHostsRegistry(registry)

	// Get Prometheus timeout header
	collector.PrometheusTimeout, _ = strconv.ParseFloat(r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"), 64)

	expiredCacheParam := r.URL.Query().Get("expired-cache")
	if expiredCacheParam != "" {
		if !collector.CacheConfig.Enabled {
			http.Error(w, "cache not enabled", http.StatusBadRequest)
			return
		}

		var err error
		collector.UseExpiredCache, err = strconv.ParseBool(expiredCacheParam)
		if err != nil {
			http.Error(w, "expired-cache should be a boolean", http.StatusBadRequest)
			return
		}
	}

	cacheParam := r.URL.Query().Get("cache")
	if cacheParam != "" {
		if !collector.CacheConfig.Enabled {
			http.Error(w, "cache not enabled", http.StatusBadRequest)
			return
		}

		var err error
		collector.UseCache, err = strconv.ParseBool(cacheParam)
		if err != nil {
			http.Error(w, "cache should be a boolean", http.StatusBadRequest)
			return
		}
	}

	registry.MustRegister(collector)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}
