package main

import (
	"context"
	"embed"
	"html/template"
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

func indexHandler(httpPathPrefix string, content *IndexPageContent) http.HandlerFunc {
	templ := template.New("main")
	templ.Funcs(map[string]interface{}{
		"AddPathPrefix": func(link string) string {
			return path.Join(httpPathPrefix, link)
		},
	})
	template.Must(templ.Parse(indexPageHTML))

	return func(w http.ResponseWriter, _ *http.Request) {
		err := templ.Execute(w, indexPageContents{LinkGroups: content.GetContent()})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
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
		var err error
		collector.UseExpiredCache, err = strconv.ParseBool(expiredCacheParam)
		if err != nil {
			http.Error(w, "expired-cache should be a boolean", http.StatusBadRequest)
			return
		}
	}

	cacheParam := r.URL.Query().Get("cache")
	if cacheParam != "" {
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

	registry.MustRegister(collector)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}
