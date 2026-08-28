package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fgouteroux/foreman_exporter/memcache"
)

// hostFactsServer serves the thin host list plus the per-host facts endpoint,
// failing the facts of the host ids listed in failIDs with a 429.
func hostFactsServer(n int, failIDs map[int]bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/facts") {
			var id int
			_, _ = fmt.Sscanf(r.URL.Path, "/api/v2/hosts/%d/facts", &id)
			if failIDs[id] {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprintf(w, `{"total":2,"page":1,"per_page":1000,"results":{"host-%d":{"os.name":"RedHat","kernel-version":"5.14"}}}`, id)
			return
		}

		results := make([]string, 0, n)
		for i := 0; i < n; i++ {
			results = append(results, fmt.Sprintf(`{"id":%d,"name":"host-%d"}`, i, i))
		}
		fmt.Fprintf(w, `{"total":%d,"subtotal":%d,"page":1,"per_page":1000,"results":[%s]}`,
			n, n, strings.Join(results, ","))
	}))
}

func newHostFactCollector(t *testing.T, ts *httptest.Server) HostFactCollector {
	t.Helper()
	return HostFactCollector{
		Client:      testClient(t, ts),
		Logger:      testLogger(),
		CacheConfig: &cacheConfig{},
		Timeout:     30,
	}
}

func TestHostFactCollectorExportsPartialResult(t *testing.T) {
	// One host out of three is rate-limited. Its facts are lost, the other two
	// must still be exported: dropping everything is what made a single 429
	// throw away a multi-minute scrape.
	ts := hostFactsServer(3, map[int]bool{2: true})
	defer ts.Close()

	mfs := gather(t, newHostFactCollector(t, ts))

	mf, ok := mfs["foreman_exporter_host_facts_info"]
	if !ok {
		t.Fatal("no host fact series exported for a partial scrape")
	}
	if n := len(mf.GetMetric()); n != 2 {
		t.Fatalf("exported %d hosts, want the 2 that succeeded", n)
	}
	if v := gaugeValue(t, mfs, "foreman_exporter_host_facts_scrape_error"); v != 1 {
		t.Fatalf("scrape_error = %v, want 1 on a partial scrape", v)
	}
	// Fact names are sanitised into valid label names.
	var got []string
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		got = append(got, lp.GetName())
	}
	want := map[string]bool{"name": true, "os_name": true, "kernel_version": true}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected label %q in %v", name, got)
		}
	}
}

func TestHostFactCollectorDoesNotCachePartialByDefault(t *testing.T) {
	ts := hostFactsServer(3, map[int]bool{2: true})
	defer ts.Close()

	prev := localCache
	localCache = memcache.NewLocalCache()
	defer func() { localCache = prev }()

	c := newHostFactCollector(t, ts)
	c.CacheConfig = &cacheConfig{Enabled: true, ExpiresTTL: time.Hour}
	c.UseCache = true

	gather(t, c)
	if _, found := localCache.Get(hostsFactsKey); found {
		t.Fatal("a partial scrape refreshed the cache without --collector.hostfact.cache.update-on-partial")
	}

	c.CacheOnPartial = true
	gather(t, c)
	if _, found := localCache.Get(hostsFactsKey); !found {
		t.Fatal("cache not refreshed from a partial scrape despite update-on-partial")
	}
}

func TestHostFactCollectorFallsBackToExpiredCache(t *testing.T) {
	// Total failure: the cached (expired) value must be served rather than
	// clobbered by the empty scrape result.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer ts.Close()

	prev := localCache
	localCache = memcache.NewLocalCache()
	defer func() { localCache = prev }()
	localCache.Set(hostsFactsKey, []map[string]string{{"name": "host-0", "os_name": "RedHat"}}, -time.Minute)

	c := newHostFactCollector(t, ts)
	c.CacheConfig = &cacheConfig{Enabled: true, ExpiresTTL: time.Hour}
	c.UseCache = true
	c.UseExpiredCache = true

	mfs := gather(t, c)
	mf, ok := mfs["foreman_exporter_host_facts_info"]
	if !ok || len(mf.GetMetric()) != 1 {
		t.Fatalf("expired cache not served on scrape failure: %v", mfs["foreman_exporter_host_facts_info"])
	}
	if v := gaugeValue(t, mfs, "foreman_exporter_host_facts_use_expired_cache"); v != 1 {
		t.Fatalf("use_expired_cache = %v, want 1", v)
	}

	// Without the opt-in, nothing is exported.
	c.UseExpiredCache = false
	if mf, ok := gather(t, c)["foreman_exporter_host_facts_info"]; ok {
		t.Fatalf("expired cache served without --expired-cache: %d series", len(mf.GetMetric()))
	}
}

func TestHostFactCollectorNoDataMessages(t *testing.T) {
	// "cache is empty" is a conclusion the collector is only entitled to draw
	// when it actually looked. With ?cache=false it never reads the cache, so
	// saying it is empty is simply false.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer ts.Close()

	prev := localCache
	localCache = memcache.NewLocalCache()
	defer func() { localCache = prev }()
	localCache.Set(hostsFactsKey, []map[string]string{{"name": "host-0"}}, -time.Minute)

	tests := []struct {
		name       string
		cacheOn    bool
		useCache   bool
		wantSubstr string
	}{
		{name: "cache bypassed by the request", cacheOn: true, useCache: false, wantSubstr: "bypassed by the request"},
		{name: "cache disabled", cacheOn: false, useCache: false, wantSubstr: "cache is disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := newHostFactCollector(t, ts)
			c.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			c.CacheConfig = &cacheConfig{Enabled: tc.cacheOn, ExpiresTTL: time.Hour}
			c.UseCache = tc.useCache

			gather(t, c)

			got := buf.String()
			if strings.Contains(got, "cache is empty") {
				t.Fatalf("logged %q; the collector never read the cache, so it cannot claim it is empty", got)
			}
			if !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("logged %q, want it to mention %q", got, tc.wantSubstr)
			}
		})
	}
}
