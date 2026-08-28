package foreman

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

var (
	factValuesHistVecMetric = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "foreman_exporter_client_fact_values_request_duration_seconds",
			Help: "A histogram of /api/v2/fact_values request latencies from the foreman client.",
			// A request costs a few seconds when the response is comfortably
			// under a page and an order of magnitude more when it fills one, so
			// the buckets have to reach well past a minute: stopping at 60s puts
			// the whole degraded regime in +Inf, where no quantile is usable.
			Buckets: []float64{0.5, 1, 2.5, 5, 7.5, 10, 15, 30, 60, 90, 120, 300},
		},
		[]string{"status"},
	)
	factValuesCounterMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "foreman_exporter_client_fact_values_requests_total",
			Help: "A counter for /api/v2/fact_values requests from the foreman client.",
		},
		[]string{"status"},
	)
	factValuesRowsMetric = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "foreman_exporter_host_facts_page_rows",
			Help:    "A histogram of the fact rows returned per fact_values response (a batch may span several).",
			Buckets: []float64{100, 500, 1000, 2500, 5000, 7500, 9000, 10000, 20000, 50000},
		},
	)
	factValuesPageFillMetric = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name: "foreman_exporter_host_facts_page_fill_ratio",
			Help: "How full each fact_values response was, as rows over the per_page the server applied. Alertable without knowing the configured per-page; reaching 1 means the batch had to be continued.",
			// The interesting region is the approach to a full page, so the
			// buckets tighten near 1 rather than being evenly spaced.
			Buckets: []float64{0.1, 0.25, 0.5, 0.7, 0.8, 0.9, 0.95, 0.99, 1},
		},
	)
	factValuesPagesContinuedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		// Named for what it is: a page came back full so the next one was
		// fetched. That is nominal, not a failure. The genuinely truncated case
		// is host_facts_batches_maxpages_total.
		Name: "foreman_exporter_host_facts_pages_continued_total",
		Help: "A counter of fact_values responses that filled a page and were therefore continued on the next one.",
	})
	factValuesClampedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_host_facts_per_page_clamped_total",
		Help: "A counter of fact_values responses where foreman applied a lower per_page than requested.",
	})
	factValuesBatchesMetric = prometheus.NewCounter(prometheus.CounterOpts{
		// The denominator: without it the failure counters have no scale and any
		// absolute alert threshold breaks as soon as the fleet grows.
		Name: "foreman_exporter_host_facts_batches_total",
		Help: "A counter of fact_values batches attempted.",
	})
	factValuesBatchFailedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_host_facts_batches_failed_total",
		Help: "A counter of fact_values batches that failed; only their hosts are missing from the result.",
	})
	factValuesMaxPagesMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_host_facts_batches_maxpages_total",
		Help: "A counter of fact_values batches that hit max-pages and are therefore incomplete; lower batch-size or raise per-page.",
	})
	factValuesHostsLostMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_host_facts_hosts_lost_total",
		Help: "A counter of hosts whose facts could not be collected because their batch failed or was truncated. Not reconstructable from the batch counters, since batches are shrunk to fit the request line.",
	})
	hostListDurationMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "foreman_exporter_host_facts_host_list_duration_seconds",
		Help: "Duration of the thin host list fetch that precedes the fact collection. The collector's total duration is this plus the fact phase; without the split, a slow list is indistinguishable from slow facts.",
	})
	factValuesNoFactsMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "foreman_exporter_host_facts_hosts_without_facts",
		Help: "Hosts that were asked for and came back with no matching fact on the last collection. Expected for hosts that never reported; a jump means facts stopped matching the search.",
	})
	factValuesBatchHostsMetric = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "foreman_exporter_host_facts_batch_hosts",
			Help:    "A histogram of how many hosts each batch actually carried. Batches are shrunk to fit the request line, and a silent collapse towards 1 host per batch is exactly the regression the bulk mode removes.",
			Buckets: []float64{1, 2, 5, 10, 20, 50, 100, 250, 500},
		},
	)
)

// Fallbacks used when the caller leaves the field at zero. They mirror the CLI
// defaults in main.go so the library and the binary behave the same.
const (
	defaultFactPerPage  = 10000
	defaultFactMaxPages = 10
)

// BatchFactsResult is what one batch of host ids brought back. Facts holds the
// raw, unfiltered facts keyed by hostname; HostIDs is what the batch asked for,
// which is what lets the caller tell "this host has no matching fact" apart
// from "this host was lost".
type BatchFactsResult struct {
	HostIDs []int64
	Facts   map[string]map[string]string
	// Missing lists the hosts the batch asked for that came back with no fact at
	// all. Knowing the ids that were requested is the whole point of batching by
	// host: without it, a host absent from the response is indistinguishable
	// from a host that simply has nothing matching the search.
	Missing   []string
	Pages     int
	Truncated bool
	Err       error
}

// escapeSearchValue quotes a scoped_search value when it contains anything the
// query tokenizer would treat as syntax. Fact names are usually plain
// (foo::bar), but nothing guarantees it and an unquoted stray character
// silently changes the query rather than failing it.
func escapeSearchValue(v string) string {
	if !strings.ContainsAny(v, " \"'()^!~<>=&|,-\\`") {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// buildFactSearch assembles the scoped_search query sent to /api/v2/fact_values.
//
// inOp is the "in" operator: "^" on any recent foreman, "or" as a fallback for
// servers that reject it (expands to `host_id = a or host_id = b`, roughly
// three times longer, so batches have to be smaller).
func buildFactSearch(userSearch, inOp string, ids []int64, factNames []string) string {
	var parts []string

	if userSearch != "" {
		parts = append(parts, "("+userSearch+")")
	}

	if len(factNames) > 0 {
		quoted := make([]string, 0, len(factNames))
		for _, n := range factNames {
			// An empty name would render as an empty term between two commas,
			// which scoped_search does not reject but does not mean anything
			// either.
			if n == "" {
				continue
			}
			quoted = append(quoted, escapeSearchValue(n))
		}
		if len(quoted) > 0 {
			parts = append(parts, "fact ^ ("+strings.Join(quoted, ", ")+")")
		}
	}

	if len(ids) > 0 {
		s := make([]string, len(ids))
		for i, id := range ids {
			s[i] = strconv.FormatInt(id, 10)
		}
		if inOp == "or" {
			for i := range s {
				s[i] = "host_id = " + s[i]
			}
			parts = append(parts, "("+strings.Join(s, " or ")+")")
		} else {
			parts = append(parts, "host_id ^ ("+strings.Join(s, ", ")+")")
		}
	}

	return strings.Join(parts, " and ")
}

// searchURLLen is the encoded length the search alone will take in the query
// string, which is what the server's request-line limit actually sees.
func searchURLLen(search string) int {
	return len(url.QueryEscape(search))
}

// splitHosts cuts the fleet into batches of at most FactBatchSize hosts, and
// shrinks a batch further when its encoded search would exceed MaxURLLength.
// Going over the server's request-line limit turns into an opaque 414 rather
// than a useful error, so the client keeps itself under it.
func (c *HTTPClient) splitHosts(hosts []Host) [][]Host {
	size := int(c.FactBatchSize)
	if size < 1 {
		size = 1
	}

	var batches [][]Host
	for start := 0; start < len(hosts); {
		end := start + size
		if end > len(hosts) {
			end = len(hosts)
		}
		// Shrink until the query fits. Halving converges in a few steps and
		// keeps the batches even, where removing one host at a time would not.
		for end > start+1 {
			ids := make([]int64, 0, end-start)
			for _, h := range hosts[start:end] {
				ids = append(ids, h.ID)
			}
			if searchURLLen(buildFactSearch(c.SearchHostFact, c.FactInOperator, ids, c.FactNames)) <= c.MaxURLLength {
				break
			}
			end = start + (end-start)/2
		}
		batches = append(batches, hosts[start:end])
		start = end
	}
	return batches
}

// GetFactValues fetches one page of /api/v2/fact_values.
func (c *HTTPClient) GetFactValues(ctx context.Context, search string, page, perPage int64) (HostFactsResponse, error) {
	var result HostFactsResponse

	params := url.Values{}
	params.Set("search", search)
	params.Add("page", fmt.Sprintf("%d", page))
	params.Add("per_page", fmt.Sprintf("%d", perPage))

	factURL, _ := url.ParseRequestURI(c.BaseURL.String())
	factURL.Path = "api/v2/fact_values"
	factURL.RawQuery = params.Encode()

	req, err := newJSONRequest(ctx, factURL.String(), c.Username, c.Password)
	if err != nil {
		return result, err
	}

	start := time.Now()
	err = c.DoWithContext(ctx, req, &result)
	elapsed := time.Since(start).Seconds()

	if err != nil {
		// Labelled by status so a rising quantile can be read as "slow" rather
		// than being polluted by timeouts, which are a different problem.
		factValuesHistVecMetric.WithLabelValues("failed").Observe(elapsed)
		factValuesCounterMetric.WithLabelValues("failed").Inc()
		var errResult ErrorResult
		if jsonErr := unmarshalErrorResult(err, &errResult); jsonErr != nil {
			c.logWarn("failed to get fact values", logrus.Fields{"path": req.URL.Path, "err": err.Error()})
		} else {
			c.logWarn("failed to get fact values", logrus.Fields{
				"path": req.URL.Path, "status": errResult.Status, "err": errResult.Error,
			})
		}
		return result, err
	}
	factValuesHistVecMetric.WithLabelValues("success").Observe(elapsed)
	factValuesCounterMetric.WithLabelValues("success").Inc()
	return result, nil
}

// GetFactValuesBatch fetches every fact of the given hosts in one request,
// following pages when the response fills one.
func (c *HTTPClient) GetFactValuesBatch(ctx context.Context, hosts []Host) BatchFactsResult {
	ids := make([]int64, len(hosts))
	for i, h := range hosts {
		ids[i] = h.ID
	}
	res := BatchFactsResult{HostIDs: ids, Facts: make(map[string]map[string]string)}

	search := buildFactSearch(c.SearchHostFact, c.FactInOperator, ids, c.FactNames)
	perPage := c.FactPerPage
	if perPage <= 0 {
		perPage = defaultFactPerPage
	}
	maxPages := int(c.FactMaxPages)
	if maxPages <= 0 {
		maxPages = defaultFactMaxPages
	}

	for page := int64(1); int(page) <= maxPages; page++ {
		resp, err := c.GetFactValues(ctx, search, page, perPage)
		if err != nil {
			res.Err = err
			res.Pages = int(page) - 1
			return res
		}

		// Foreman echoes the per_page it actually applied. Comparing the row
		// count against the requested value instead would miss a clamp and stop
		// paging early, silently dropping the rest of the batch.
		effPerPage := resp.PerPage
		if effPerPage <= 0 {
			effPerPage = perPage
		}
		if effPerPage < perPage {
			factValuesClampedMetric.Inc()
		}

		rows := 0
		for host, facts := range resp.Results {
			// Merge rather than assign: pagination cuts on fact rows, so a host
			// can straddle two pages.
			if res.Facts[host] == nil {
				res.Facts[host] = make(map[string]string, len(facts))
			}
			for k, v := range facts {
				res.Facts[host][k] = v
			}
			rows += len(facts)
		}
		factValuesRowsMetric.Observe(float64(rows))
		factValuesPageFillMetric.Observe(float64(rows) / float64(effPerPage))
		res.Pages = int(page)

		if int64(rows) < effPerPage {
			res.Missing = missingHosts(hosts, res.Facts)
			return res
		}
		factValuesPagesContinuedMetric.Inc()
	}

	res.Truncated = true
	res.Missing = missingHosts(hosts, res.Facts)
	return res
}

// joinCapped renders at most n names, saying how many it left out.
func joinCapped(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:n], ", "), len(names)-n)
}

// missingHosts returns the names of the hosts that were asked for but carry no
// fact in the response.
func missingHosts(hosts []Host, facts map[string]map[string]string) []string {
	var missing []string
	for _, h := range hosts {
		if _, ok := facts[h.Name]; !ok {
			missing = append(missing, h.Name)
		}
	}
	return missing
}

// getHostsFactsBulk collects the facts of every host through /api/v2/fact_values,
// one request per batch of host ids instead of one request per host.
//
// A failed batch only costs its own hosts: the others are still returned, along
// with an error naming how many were lost.
func (c *HTTPClient) getHostsFactsBulk(ctx context.Context, hosts []Host) (map[string]map[string]string, error) {
	batches := c.splitHosts(hosts)
	c.logInfof("collecting facts for %d hosts in %d batches", len(hosts), len(batches))

	concurrency := c.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	var (
		mu         sync.Mutex
		hostsFacts = make(map[string]map[string]string, len(hosts))
		failed     int
		truncated  int
		lostHosts  int
		noFacts    int
		firstErr   error
		wg         sync.WaitGroup
		sem        = make(chan struct{}, concurrency)
	)

	for _, batch := range batches {
		wg.Add(1)
		go func(batch []Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := c.GetFactValuesBatch(ctx, batch)
			factValuesBatchesMetric.Inc()
			factValuesBatchHostsMetric.Observe(float64(len(batch)))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case res.Err != nil:
				factValuesBatchFailedMetric.Inc()
				factValuesHostsLostMetric.Add(float64(len(batch)))
				failed++
				lostHosts += len(batch)
				if firstErr == nil {
					firstErr = res.Err
				}
				// The pages that did come back are dropped on purpose. Pagination
				// cuts on fact rows, so a host straddling the failure point would
				// be exported with only part of its facts — and facts are const
				// labels, so a shorter label set is a *different* time series, not
				// a smaller one. Losing the batch for one pass is recoverable;
				// silently replacing series is not.
				return
			case res.Truncated:
				// The batch ran out of pages before foreman ran out of rows, so
				// the facts it returned are incomplete. Surfacing it as an error
				// is what keeps it from silently shrinking the exported labels
				// and, worse, being written to the cache.
				factValuesMaxPagesMetric.Inc()
				factValuesHostsLostMetric.Add(float64(len(batch)))
				truncated++
				lostHosts += len(batch)
				// Same reasoning as above: the hosts in a truncated batch may be
				// missing facts, so none of them are exported.
				return
			}
			// A host asked for and absent from the response has no matching fact.
			// That is legitimate — a host that never reported, or whose facts do
			// not match the search — so it is counted and logged, not treated as
			// an error. What matters is being able to see it move.
			noFacts += len(res.Missing)
			if len(res.Missing) > 0 {
				// Name them: the gauge says the number moved, only the names say
				// which hosts to go and look at. Capped so one bad batch cannot
				// flood the log with a whole fleet.
				c.logDebugf("%d hosts have no matching fact: %s", len(res.Missing), joinCapped(res.Missing, 20))
			}
			for name, facts := range res.Facts {
				hostsFacts[name] = c.filterFacts(facts)
			}
		}(batch)
	}
	wg.Wait()
	factValuesNoFactsMetric.Set(float64(noFacts))

	switch {
	case failed > 0 && truncated > 0:
		return hostsFacts, fmt.Errorf("incomplete host facts: %d/%d batches failed (%d hosts not collected, first error: %v) and %d hit max-pages",
			failed, len(batches), lostHosts, firstErr, truncated)
	case failed > 0:
		return hostsFacts, fmt.Errorf("incomplete host facts: %d/%d batches failed, %d hosts not collected (first error: %v)",
			failed, len(batches), lostHosts, firstErr)
	case truncated > 0:
		return hostsFacts, fmt.Errorf("incomplete host facts: %d/%d batches hit max-pages, %d hosts not collected; lower batch-size or raise per-page",
			truncated, len(batches), lostHosts)
	}
	return hostsFacts, nil
}

// filterFacts applies the client-side include/exclude regexes and drops empty
// values. It stays even when the names are pushed server-side: the server
// filter is an optimisation, this is what defines the exported label set.
func (c *HTTPClient) filterFacts(data map[string]string) map[string]string {
	factsMap := make(map[string]string, len(data))
	for k, v := range data {
		if c.IncludeHostFactRegex != nil && len(c.IncludeHostFactRegex.FindStringSubmatch(k)) == 0 {
			continue
		}
		if c.ExcludeHostFactRegex != nil && len(c.ExcludeHostFactRegex.FindStringSubmatch(k)) != 0 {
			continue
		}
		if v != "" {
			factsMap[k] = v
		}
	}
	return factsMap
}

// SortedFactNames is only used to keep the search stable across runs, which
// makes the queries comparable in foreman's own logs.
func SortedFactNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}
