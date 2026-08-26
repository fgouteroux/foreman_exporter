package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/fgouteroux/foreman_exporter/foreman"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testClient(t *testing.T, ts *httptest.Server) *foreman.HTTPClient {
	t.Helper()
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	return foreman.NewHTTPClient(foreman.ClientConfig{
		BaseURL:     base,
		Concurrency: 2,
	})
}

// gather collects the collector into a throwaway registry and returns the
// families keyed by metric name.
func gather(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

func gaugeValue(t *testing.T, mfs map[string]*dto.MetricFamily, name string) float64 {
	t.Helper()
	mf, ok := mfs[name]
	if !ok || len(mf.GetMetric()) == 0 {
		t.Fatalf("metric %q not collected", name)
	}
	return mf.GetMetric()[0].GetGauge().GetValue()
}

// labelNames returns the sorted label names of the first series of a family.
func labelNames(t *testing.T, mfs map[string]*dto.MetricFamily, name string) []string {
	t.Helper()
	mf, ok := mfs[name]
	if !ok || len(mf.GetMetric()) == 0 {
		t.Fatalf("metric %q not collected", name)
	}
	var names []string
	for _, lp := range mf.GetMetric()[0].GetLabel() {
		names = append(names, lp.GetName())
	}
	return names
}

// hostsServer serves /api/v2/hosts with n identical-shaped hosts.
func hostsServer(n int, failPage string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		perPage, page := q.Get("per_page"), q.Get("page")
		if perPage == "1" {
			fmt.Fprintf(w, `{"total":%d,"subtotal":%d,"page":1,"per_page":1,"results":[]}`, n, n)
			return
		}
		if page == failPage {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		var pp, pn int
		_, _ = fmt.Sscan(perPage, &pp)
		_, _ = fmt.Sscan(page, &pn)
		first := (pn - 1) * pp
		results := make([]string, 0, pp)
		for i := first; i < first+pp && i < n; i++ {
			results = append(results, fmt.Sprintf(
				`{"id":%d,"name":"host-%d","global_status_label":"OK","environment_name":"prod","hostgroup_name":"web","owner_name":"ops","model_name":"kvm"}`, i, i))
		}
		fmt.Fprintf(w, `{"total":%d,"subtotal":%d,"page":%s,"per_page":%s,"results":[%s]}`,
			n, n, page, perPage, strings.Join(results, ","))
	}))
}

func newHostCollector(t *testing.T, ts *httptest.Server) HostCollector {
	t.Helper()
	return HostCollector{
		Client:      testClient(t, ts),
		Logger:      testLogger(),
		CacheConfig: &cacheConfig{},
		Timeout:     30,
	}
}

func TestHostCollectorAppliesLabelFilters(t *testing.T) {
	ts := hostsServer(3, "")
	defer ts.Close()

	c := newHostCollector(t, ts)
	c.ExcludeHostLabelRegex = regexp.MustCompile("^(environment|hostgroup|model|owner)$")

	got := labelNames(t, gather(t, c), "foreman_exporter_host_status_info")
	for _, dropped := range []string{"environment", "hostgroup", "model", "owner"} {
		for _, name := range got {
			if name == dropped {
				t.Fatalf("label %q still exported despite labels-exclude; got %v", dropped, got)
			}
		}
	}
	// The labels that were not excluded are still there.
	var hasName, hasStatus bool
	for _, name := range got {
		hasName = hasName || name == "name"
		hasStatus = hasStatus || name == "global_status"
	}
	if !hasName || !hasStatus {
		t.Fatalf("kept labels missing, got %v", got)
	}
}

func TestHostCollectorIncludeFilterKeepsNameAndMatches(t *testing.T) {
	ts := hostsServer(2, "")
	defer ts.Close()

	c := newHostCollector(t, ts)
	c.IncludeHostLabelRegex = regexp.MustCompile("^(global_status)$")

	got := labelNames(t, gather(t, c), "foreman_exporter_host_status_info")
	if len(got) != 2 {
		t.Fatalf("labels = %v, want only name and global_status", got)
	}
}

func TestHostCollectorExportsPartialList(t *testing.T) {
	// Page 2 of 3 fails: the hosts from the other pages must still be exported
	// instead of the whole scrape being dropped.
	ts := hostsServer(250, "2")
	defer ts.Close()

	mfs := gather(t, newHostCollector(t, ts))

	mf, ok := mfs["foreman_exporter_host_status_info"]
	if !ok {
		t.Fatal("no host series exported for a partial list")
	}
	if n := len(mf.GetMetric()); n != 150 {
		t.Fatalf("exported %d hosts, want the 150 from the successful pages", n)
	}
	if v := gaugeValue(t, mfs, "foreman_exporter_host_scrape_error"); v != 1 {
		t.Fatalf("scrape_error = %v, want 1 on a partial list", v)
	}
}
