package foreman

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func bulkClient(t *testing.T, ts *httptest.Server, cfg func(*ClientConfig)) *HTTPClient {
	t.Helper()
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	c := ClientConfig{
		BaseURL: base, Concurrency: 2, BulkFacts: true,
		FactBatchSize: 100, FactPerPage: 1000, FactMaxPages: 10,
		FactInOperator: "^", MaxURLLength: 6000,
	}
	if cfg != nil {
		cfg(&c)
	}
	return NewHTTPClient(c)
}

func TestBuildFactSearch(t *testing.T) {
	tests := []struct {
		name       string
		userSearch string
		inOp       string
		ids        []int64
		factNames  []string
		want       string
	}{
		{name: "ids only", inOp: "^", ids: []int64{1, 2}, want: "host_id ^ (1, 2)"},
		{
			name: "search and names and ids", userSearch: "a or b", inOp: "^",
			ids: []int64{7}, factNames: []string{"os", "net::ip"},
			want: "(a or b) and fact ^ (os, net::ip) and host_id ^ (7)",
		},
		{
			// The fallback for servers that reject the in operator.
			name: "or fallback", inOp: "or", ids: []int64{1, 2, 3},
			want: "(host_id = 1 or host_id = 2 or host_id = 3)",
		},
		{
			// A value carrying query syntax must be quoted, otherwise it silently
			// changes the meaning of the search instead of failing.
			name: "quotes risky names", inOp: "^", ids: []int64{1},
			factNames: []string{"plain", "weird name", "has,comma"},
			want:      `fact ^ (plain, "weird name", "has,comma") and host_id ^ (1)`,
		},
		{name: "nothing", inOp: "^", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFactSearch(tc.userSearch, tc.inOp, tc.ids, tc.factNames)
			if got != tc.want {
				t.Fatalf("buildFactSearch() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestSplitHostsRespectsBatchAndURLLimit(t *testing.T) {
	hosts := make([]Host, 1000)
	for i := range hosts {
		hosts[i] = Host{ID: int64(1000000 + i)}
	}

	c := &HTTPClient{FactBatchSize: 250, FactInOperator: "^", MaxURLLength: 6000}
	batches := c.splitHosts(hosts)
	seen := map[int64]int{}
	for _, b := range batches {
		if len(b) > 250 {
			t.Fatalf("batch of %d hosts, want at most 250", len(b))
		}
		ids := make([]int64, len(b))
		for i, h := range b {
			ids[i] = h.ID
			seen[h.ID]++
		}
		if n := searchURLLen(buildFactSearch("", "^", ids, nil)); n > 6000 {
			t.Fatalf("encoded search is %d bytes, over the 6000 limit", n)
		}
	}
	if len(seen) != len(hosts) {
		t.Fatalf("covered %d hosts, want %d", len(seen), len(hosts))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("host %d appears %d times, want exactly once", id, n)
		}
	}

	// A tight URL budget must shrink the batches rather than overflow them.
	c.MaxURLLength = 200
	for _, b := range c.splitHosts(hosts) {
		ids := make([]int64, len(b))
		for i, h := range b {
			ids[i] = h.ID
		}
		if n := searchURLLen(buildFactSearch("", "^", ids, nil)); n > 200 && len(b) > 1 {
			t.Fatalf("batch of %d hosts encodes to %d bytes, over the 200 limit", len(b), n)
		}
	}
}

// factValuesServer serves /api/v2/fact_values, handing out factsPerHost facts
// for every host id in the search, paginated on rows like foreman does.
type factValuesServer struct {
	factsPerHost int
	perPageCap   int64
	hits         atomic.Int64
	failOnce     atomic.Bool
	alwaysFull   atomic.Bool
}

func (s *factValuesServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.failOnce.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		q := r.URL.Query()
		var perPage, page int64
		fmt.Sscan(q.Get("per_page"), &perPage)
		fmt.Sscan(q.Get("page"), &page)
		if s.perPageCap > 0 && perPage > s.perPageCap {
			perPage = s.perPageCap
		}

		// every id in the search, in order
		var ids []int64
		for _, f := range strings.FieldsFunc(q.Get("search"), func(r rune) bool {
			return r < '0' || r > '9'
		}) {
			var id int64
			fmt.Sscan(f, &id)
			ids = append(ids, id)
		}

		// flatten to rows, then cut the requested page out of them
		type row struct{ host, fact string }
		var rows []row
		for _, id := range ids {
			for i := 0; i < s.factsPerHost; i++ {
				rows = append(rows, row{fmt.Sprintf("host-%d", id), fmt.Sprintf("fact_%d", i)})
			}
		}
		if s.alwaysFull.Load() {
			// every page comes back full, as if the batch never ends
			out := map[string]map[string]string{"host-1": {}}
			for i := int64(0); i < perPage; i++ {
				out["host-1"][fmt.Sprintf("fact_%d_%d", page, i)] = "v"
			}
			body, _ := json.Marshal(map[string]interface{}{
				"total": 1 << 30, "page": page, "per_page": perPage, "results": out,
			})
			_, _ = w.Write(body)
			return
		}

		start := (page - 1) * perPage
		end := start + perPage
		if start > int64(len(rows)) {
			start = int64(len(rows))
		}
		if end > int64(len(rows)) {
			end = int64(len(rows))
		}
		out := map[string]map[string]string{}
		for _, rw := range rows[start:end] {
			if out[rw.host] == nil {
				out[rw.host] = map[string]string{}
			}
			out[rw.host][rw.fact] = "v"
		}
		body, _ := json.Marshal(map[string]interface{}{
			"total": len(rows), "page": page, "per_page": perPage, "results": out,
		})
		_, _ = w.Write(body)
	}
}

func TestGetFactValuesBatchMergesPages(t *testing.T) {
	// 3 hosts x 4 facts = 12 rows over a per_page of 5: the batch spans three
	// pages and a host straddles a page boundary, which is exactly the case a
	// naive assignment (instead of a merge) would silently truncate.
	srv := &factValuesServer{factsPerHost: 4}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := bulkClient(t, ts, func(cfg *ClientConfig) { cfg.FactPerPage = 5 })
	res := c.GetFactValuesBatch(context.Background(), []Host{{ID: 1}, {ID: 2}, {ID: 3}})

	if res.Err != nil {
		t.Fatalf("batch returned error: %v", res.Err)
	}
	if len(res.Facts) != 3 {
		t.Fatalf("got %d hosts, want 3", len(res.Facts))
	}
	for host, facts := range res.Facts {
		if len(facts) != 4 {
			t.Fatalf("%s has %d facts, want 4 (a page boundary lost some)", host, len(facts))
		}
	}
	if res.Pages < 3 {
		t.Fatalf("fetched %d pages, want at least 3", res.Pages)
	}
}

func TestGetFactValuesBatchHonorsServerPerPage(t *testing.T) {
	// The server caps per_page at 5 while the client asked for 1000. Comparing
	// the row count against the requested value would see a short page and stop
	// after the first one, dropping the rest of the batch.
	srv := &factValuesServer{factsPerHost: 4, perPageCap: 5}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := bulkClient(t, ts, func(cfg *ClientConfig) { cfg.FactPerPage = 1000 })
	res := c.GetFactValuesBatch(context.Background(), []Host{{ID: 1}, {ID: 2}, {ID: 3}})

	if res.Err != nil {
		t.Fatalf("batch returned error: %v", res.Err)
	}
	if len(res.Facts) != 3 {
		t.Fatalf("got %d hosts, want 3", len(res.Facts))
	}
	for host, facts := range res.Facts {
		if len(facts) != 4 {
			t.Fatalf("%s has %d facts, want 4", host, len(facts))
		}
	}
}

func TestGetFactValuesBatchStopsOnShortPage(t *testing.T) {
	srv := &factValuesServer{factsPerHost: 2}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := bulkClient(t, ts, func(cfg *ClientConfig) { cfg.FactPerPage = 1000 })
	if res := c.GetFactValuesBatch(context.Background(), []Host{{ID: 1}}); res.Err != nil {
		t.Fatalf("batch returned error: %v", res.Err)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1 for a short page", n)
	}
}

func TestGetHostsFactsBulkIsolatesFailedBatches(t *testing.T) {
	// One batch fails; its hosts are missing but the others must come back,
	// along with an error that says how many were lost.
	srv := &factValuesServer{factsPerHost: 2}
	srv.failOnce.Store(true)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	hosts := make([]Host, 4)
	for i := range hosts {
		hosts[i] = Host{ID: int64(i + 1)}
	}
	c := bulkClient(t, ts, func(cfg *ClientConfig) {
		cfg.FactBatchSize = 1
		cfg.Concurrency = 1
	})

	facts, err := c.getHostsFactsBulk(context.Background(), hosts)
	if err == nil {
		t.Fatal("getHostsFactsBulk returned nil error although a batch failed")
	}
	// Assert the substance, not just the shared prefix: all three error branches
	// start with "incomplete host facts", so matching that alone passes even
	// when the message says nothing useful.
	for _, want := range []string{"1/4 batches failed", "1 hosts not collected"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if len(facts) != 3 {
		t.Fatalf("got %d hosts, want the 3 from the batches that succeeded", len(facts))
	}
}

func TestGetHostsFactsBulkAppliesRegexFilters(t *testing.T) {
	// The include/exclude regexes still define the exported label set, even when
	// the names are also pushed server-side.
	srv := &factValuesServer{factsPerHost: 4}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := bulkClient(t, ts, func(cfg *ClientConfig) {
		cfg.IncludeHostFactRegex = mustCompile(`^(fact_0|fact_2)$`)
	})
	facts, err := c.getHostsFactsBulk(context.Background(), []Host{{ID: 1}})
	if err != nil {
		t.Fatalf("getHostsFactsBulk returned error: %v", err)
	}
	got := facts["host-1"]
	if len(got) != 2 {
		t.Fatalf("kept %d facts %v, want only fact_0 and fact_2", len(got), got)
	}
	for _, k := range []string{"fact_0", "fact_2"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s was filtered out, want it kept", k)
		}
	}
}

func mustCompile(expr string) *regexp.Regexp { return regexp.MustCompile(expr) }

func TestGetHostsFactsBulkReportsMaxPagesAsError(t *testing.T) {
	// A batch that runs out of pages returned incomplete facts. Reporting it as
	// an error is what stops it from silently shrinking the exported labels and
	// from being written to the cache.
	srv := &factValuesServer{factsPerHost: 1}
	srv.alwaysFull.Store(true)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := bulkClient(t, ts, func(cfg *ClientConfig) {
		cfg.FactPerPage = 2
		cfg.FactMaxPages = 3
	})

	facts, err := c.getHostsFactsBulk(context.Background(), []Host{{ID: 1}})
	if err == nil {
		t.Fatal("getHostsFactsBulk returned nil error although the batch was truncated")
	}
	if !strings.Contains(err.Error(), "max-pages") {
		t.Fatalf("error = %q, want it to name the max-pages limit", err)
	}
	// Nothing from a truncated batch is exported: pagination cuts on fact rows,
	// so its hosts may be missing facts, and facts are const labels — a shorter
	// label set is a different series, not a smaller one.
	if len(facts) != 0 {
		t.Fatalf("exported %d hosts from a truncated batch, want none", len(facts))
	}
	if !strings.Contains(err.Error(), "1 hosts not collected") {
		t.Fatalf("error = %q, want it to report the hosts actually lost", err)
	}
	if n := srv.hits.Load(); n != 3 {
		t.Fatalf("server saw %d requests, want exactly the 3 allowed pages", n)
	}
}

func TestGetFactValuesBatchReportsHostsWithoutFacts(t *testing.T) {
	// The point of batching by host id is knowing what was asked for: a host
	// absent from the response must be distinguishable from a host that was
	// never requested. The fake only knows about ids it sees in the search, and
	// host 99 is given a name it will never return facts for.
	srv := &factValuesServer{factsPerHost: 2}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := bulkClient(t, ts, nil)
	res := c.GetFactValuesBatch(context.Background(), []Host{
		{ID: 1, Name: "host-1"},
		{ID: 2, Name: "unknown-host"}, // asked for, will not come back under this name
	})

	if res.Err != nil {
		t.Fatalf("batch returned error: %v", res.Err)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "unknown-host" {
		t.Fatalf("Missing = %v, want exactly [unknown-host]", res.Missing)
	}
}

func TestBuildFactSearchSkipsEmptyNames(t *testing.T) {
	// An empty name would render as an empty term between two commas, which is
	// not rejected by scoped_search and not meaningful either.
	got := buildFactSearch("", "^", []int64{1}, []string{"os", "", "net"})
	want := "fact ^ (os, net) and host_id ^ (1)"
	if got != want {
		t.Fatalf("buildFactSearch() = %q, want %q", got, want)
	}
	if got := buildFactSearch("", "^", []int64{1}, []string{"", ""}); got != "host_id ^ (1)" {
		t.Fatalf("all-empty names produced %q, want the fact clause to be dropped", got)
	}
}

func TestFactPerPageFallbackMatchesCLIDefault(t *testing.T) {
	// The library fallback and the CLI default must agree, otherwise using the
	// package directly behaves differently from the binary.
	if defaultFactPerPage != 10000 {
		t.Fatalf("defaultFactPerPage = %d, want 10000 to match --collector.hostfact.per-page", defaultFactPerPage)
	}
	if defaultFactMaxPages != 10 {
		t.Fatalf("defaultFactMaxPages = %d, want 10 to match --collector.hostfact.max-pages", defaultFactMaxPages)
	}
}

func TestFilterFacts(t *testing.T) {
	// filterFacts was extracted from the per-host path by this change, so both
	// of its branches need a guard: the exclude regex and the empty-value drop
	// were previously covered only by not existing anywhere else.
	tests := []struct {
		name     string
		include  string
		exclude  string
		in       map[string]string
		wantKeys []string
	}{
		{name: "no filter keeps everything", in: map[string]string{"a": "1", "b": "2"}, wantKeys: []string{"a", "b"}},
		{name: "include keeps matches", include: "^a$", in: map[string]string{"a": "1", "b": "2"}, wantKeys: []string{"a"}},
		{name: "exclude drops matches", exclude: "^b$", in: map[string]string{"a": "1", "b": "2"}, wantKeys: []string{"a"}},
		{name: "exclude wins over include", include: "^[ab]$", exclude: "^b$", in: map[string]string{"a": "1", "b": "2"}, wantKeys: []string{"a"}},
		{name: "empty values are dropped", in: map[string]string{"a": "1", "b": ""}, wantKeys: []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &HTTPClient{}
			if tc.include != "" {
				c.IncludeHostFactRegex = regexp.MustCompile(tc.include)
			}
			if tc.exclude != "" {
				c.ExcludeHostFactRegex = regexp.MustCompile(tc.exclude)
			}
			got := c.filterFacts(tc.in)
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("kept %v, want keys %v", got, tc.wantKeys)
			}
			for _, k := range tc.wantKeys {
				if _, ok := got[k]; !ok {
					t.Fatalf("kept %v, missing %q", got, k)
				}
			}
		})
	}
}

func TestEscapeSearchValueQuotesAndEscapes(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "plain", want: "plain"},
		{in: "foo::bar", want: "foo::bar"},
		{in: "has space", want: `"has space"`},
		{in: `he said "hi"`, want: `"he said \"hi\""`},
		{in: `back\slash`, want: `"back\\slash"`},
	}
	for _, tc := range tests {
		if got := escapeSearchValue(tc.in); got != tc.want {
			t.Fatalf("escapeSearchValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGetHostsFactsFilteredDispatchesOnBulkFlag(t *testing.T) {
	// The dispatch itself was untested: every other test calls the bulk path
	// directly, so BulkFacts=false silently routing to bulk would go unnoticed.
	var factValuesHits, perHostHits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/facts"):
			perHostHits.Add(1)
			_, _ = w.Write([]byte(`{"total":1,"page":1,"per_page":1000,"results":{"host-1":{"a":"1"}}}`))
		case strings.HasSuffix(r.URL.Path, "/fact_values"):
			factValuesHits.Add(1)
			_, _ = w.Write([]byte(`{"total":1,"page":1,"per_page":1000,"results":{"host-1":{"a":"1"}}}`))
		default:
			_, _ = w.Write([]byte(`{"total":1,"subtotal":1,"page":1,"per_page":1000,"results":[{"id":1,"name":"host-1"}]}`))
		}
	}))
	defer ts.Close()

	for _, bulk := range []bool{true, false} {
		factValuesHits.Store(0)
		perHostHits.Store(0)
		c := bulkClient(t, ts, func(cfg *ClientConfig) { cfg.BulkFacts = bulk })
		if _, err := c.GetHostsFactsFiltered(1000); err != nil {
			t.Fatalf("bulk=%v: %v", bulk, err)
		}
		if bulk && (factValuesHits.Load() == 0 || perHostHits.Load() != 0) {
			t.Fatalf("bulk=true routed to fact_values=%d perhost=%d", factValuesHits.Load(), perHostHits.Load())
		}
		if !bulk && (perHostHits.Load() == 0 || factValuesHits.Load() != 0) {
			t.Fatalf("bulk=false routed to fact_values=%d perhost=%d", factValuesHits.Load(), perHostHits.Load())
		}
	}
}

func TestJoinCapped(t *testing.T) {
	all := []string{"a", "b", "c"}
	if got := joinCapped(all, 5); got != "a, b, c" {
		t.Fatalf("joinCapped under the cap = %q, want the full list", got)
	}
	if got := joinCapped(all, 2); got != "a, b and 1 more" {
		t.Fatalf("joinCapped over the cap = %q, want the count of what was left out", got)
	}
}

func TestGetHostsFactsFilteredTimesTheHostListPhase(t *testing.T) {
	// The collector's duration is the host list plus the facts. Timing them
	// together makes a slow list look like slow facts, which is exactly the
	// wrong conclusion to draw when sizing batches.
	delay := 150 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fact_values") {
			_, _ = w.Write([]byte(`{"total":1,"page":1,"per_page":1000,"results":{"host-1":{"a":"1"}}}`))
			return
		}
		time.Sleep(delay) // the slow phase
		_, _ = w.Write([]byte(`{"total":1,"subtotal":1,"page":1,"per_page":1000,"results":[{"id":1,"name":"host-1"}]}`))
	}))
	defer ts.Close()

	c := bulkClient(t, ts, nil)
	if _, err := c.GetHostsFactsFiltered(1000); err != nil {
		t.Fatalf("GetHostsFactsFiltered returned error: %v", err)
	}

	got := testutil.ToFloat64(hostListDurationMetric)
	if got < delay.Seconds() {
		t.Fatalf("host list duration = %vs, want at least the %v the list took", got, delay)
	}
	if got > 5 {
		t.Fatalf("host list duration = %vs, suspiciously high: it should not include the fact phase", got)
	}
}
