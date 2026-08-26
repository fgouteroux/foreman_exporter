package foreman

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sirupsen/logrus"
)

var (
	inFlightGaugeMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "foreman_exporter_client_in_flight_requests",
		Help: "A gauge of all in-flight requests for the foreman client.",
	})

	counterMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "foreman_exporter_client_requests_total",
			Help: "A counter for all requests from the foreman client.",
		},
		[]string{"code", "method"},
	)

	histVecMetric = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "foreman_exporter_client_request_duration_seconds",
			Help:    "A histogram of all request latencies from the foreman client.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{},
	)
	hostsHistVecMetric = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "foreman_exporter_client_hosts_request_duration_seconds",
			Help:    "A histogram of hosts requests latencies from the foreman client.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{},
	)
	hostsCounterMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "foreman_exporter_client_hosts_requests_total",
			Help: "A counter for hosts requests from the foreman client.",
		},
		[]string{"status"},
	)
	hostsFactsHistVecMetric = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "foreman_exporter_client_hosts_facts_request_duration_seconds",
			Help:    "A histogram of hosts facts requests latencies from the foreman client.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{},
	)
	hostsFactsCounterMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "foreman_exporter_client_hosts_facts_requests_total",
			Help: "A counter for hosts facts requests from the foreman client.",
		},
		[]string{"status"},
	)
	retryAfterMetric = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "foreman_exporter_client_retry_after_seconds",
			Help:    "A histogram of Retry-After delays honored from foreman rate-limit responses.",
			Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600},
		},
		[]string{"status"},
	)
	UserAgent = fmt.Sprintf("foreman_exporter/%s", version.Version)
)

func init() {
	// Register metrics in the standard registry.
	prometheus.MustRegister(
		counterMetric,
		histVecMetric,
		inFlightGaugeMetric,
		retryAfterMetric,
	)
}

type ErrorResult struct {
	Status int64  `json:"status"`
	Error  string `json:"error"`
}

type HostResponse struct {
	Total int64 `json:"total"`
	// Subtotal is how many records match the search. Foreman sets it equal to
	// Total when no search is given; Total itself always counts every record,
	// so paginating on Total overshoots as soon as a search filter is set. It is
	// a pointer so that a search matching nothing (subtotal 0) is not mistaken
	// for a foreman version that does not report the field at all.
	Subtotal *int64 `json:"subtotal"`
	Page     int64  `json:"page"`
	PerPage  int64  `json:"per_page"`
	Results  []Host `json:"results"`
}

type HostFactsResponse struct {
	Total   int64                        `json:"total"`
	Page    int64                        `json:"page"`
	PerPage int64                        `json:"per_page"`
	Results map[string]map[string]string `json:"results"`
}

// a struct to hold the result from each request including an index
// which will be used for sorting the results after they come in
type HostFactsWithConcurrencyResult struct {
	Index  int
	Result HostFactsResponse
	Error  error
}

// a struct to hold the result from each request including an index
// which will be used for sorting the results after they come in
type HostWithConcurrencyResult struct {
	Index  int
	Result HostResponse
	Error  error
}

type Host struct {
	ID                       int64  `json:"id"`
	Name                     string `json:"name"`
	GlobalStatusLabel        string `json:"global_status_label,omitempty"`
	ConfigurationStatusLabel string `json:"configuration_status_label,omitempty"`
	BuildStatusLabel         string `json:"build_status_label,omitempty"`
	OrganizationName         string `json:"organization_name,omitempty"`
	EnvironmentName          string `json:"environment_name,omitempty"`
	OperatingSystemName      string `json:"operatingsystem_name,omitempty"`
	OwnerName                string `json:"owner_name,omitempty"`
	LocationName             string `json:"location_name,omitempty"`
	ModelName                string `json:"model_name,omitempty"`
	HostgroupName            string `json:"hostgroup_name,omitempty"`
}

type HTTPClient struct {
	client               *retryablehttp.Client
	BaseURL              *url.URL
	Username             string
	Password             string
	onRequestCompleted   RequestCompletionCallback
	Concurrency          int64
	Limit                int64
	Search               string
	SearchHostFact       string
	IncludeHostFactRegex *regexp.Regexp
	ExcludeHostFactRegex *regexp.Regexp
	Log                  *logrus.Logger
}

// RequestCompletionCallback defines the type of the request callback function
type RequestCompletionCallback func(*http.Request, *http.Response)

type LeveledLogrus struct {
	*logrus.Logger
}

func fields(keysAndValues []interface{}) map[string]interface{} {
	fields := make(map[string]interface{})

	for i := 0; i < len(keysAndValues)-1; i += 2 {
		fields[keysAndValues[i].(string)] = keysAndValues[i+1]
	}

	return fields
}

func (l *LeveledLogrus) Error(msg string, keysAndValues ...interface{}) {
	l.WithFields(fields(keysAndValues)).Error(msg)
}

func (l *LeveledLogrus) Info(msg string, keysAndValues ...interface{}) {
	l.WithFields(fields(keysAndValues)).Info(msg)
}
func (l *LeveledLogrus) Debug(msg string, keysAndValues ...interface{}) {
	l.WithFields(fields(keysAndValues)).Debug(msg)
}

func (l *LeveledLogrus) Warn(msg string, keysAndValues ...interface{}) {
	l.WithFields(fields(keysAndValues)).Warn(msg)
}

// retryAfterBackoff honors the Retry-After response header sent by Foreman
// when it rate-limits the client (HTTP 429 Too Many Requests or 503 Service
// Unavailable) before falling back to retryablehttp's exponential backoff.
// Unlike retryablehttp.DefaultBackoff, it accepts both forms of the header
// allowed by RFC 7231: delay-seconds ("120") and HTTP-date.
//
// The honored delay is capped at retryMaxWait (0 disables the cap): foreman can
// answer with minutes or hours, and a retry sleeping that long keeps holding a
// concurrency slot while doing nothing.
func retryAfterBackoff(log *logrus.Logger, retryMaxWait time.Duration) retryablehttp.Backoff {
	return func(minWait, maxWait time.Duration, attemptNum int, resp *http.Response) time.Duration {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusTooManyRequests, http.StatusServiceUnavailable:
				if sleep, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
					capped := retryMaxWait > 0 && sleep > retryMaxWait
					if capped {
						sleep = retryMaxWait
					}
					// Observe the delay actually waited, not the advertised one.
					retryAfterMetric.WithLabelValues(strconv.Itoa(resp.StatusCode)).Observe(sleep.Seconds())
					if log != nil {
						log.WithFields(logrus.Fields{
							"status":      resp.StatusCode,
							"retry_after": sleep.String(),
							"capped":      capped,
							"attempt":     attemptNum,
						}).Warn("honoring Retry-After header from foreman")
					}
					return sleep
				}
			}
		}
		return retryablehttp.DefaultBackoff(minWait, maxWait, attemptNum, resp)
	}
}

// parseRetryAfter parses a Retry-After header value in either the delay-seconds
// ("120") or the HTTP-date ("Wed, 21 Oct 2015 07:28:00 GMT") form. It returns
// false when the header is empty or malformed.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		// The date is in the past, retry immediately.
		return 0, true
	}
	return 0, false
}

// ClientConfig holds the foreman HTTP client settings. It replaces the long
// positional argument list NewHTTPClient used to take, so adding a knob no
// longer risks silently swapping two same-typed arguments at a call site.
type ClientConfig struct {
	BaseURL       *url.URL
	Username      string
	Password      string
	SkipTLSVerify bool

	// Concurrency caps the in-flight requests fanned out per collector.
	Concurrency int64
	// MaxConnsPerHost sizes the idle connection pool. 0 derives it from
	// Concurrency. It is not a hard cap on open connections: both collectors can
	// fan out at the same time, so capping would make them queue on the
	// transport instead of on their own semaphore.
	MaxConnsPerHost int64
	Limit           int64
	RetryMax        int64
	// RetryMaxWait caps the Retry-After delay honored on 429/503 (0 = no cap).
	RetryMaxWait time.Duration

	Search               string
	SearchHostFact       string
	IncludeHostFactRegex *regexp.Regexp
	ExcludeHostFactRegex *regexp.Regexp

	Log *logrus.Logger
}

// resolveIdleConns derives the idle pool size from the configured value,
// falling back to the concurrency with a small floor.
func resolveIdleConns(maxConnsPerHost, concurrency int64) int {
	if maxConnsPerHost > 0 {
		return int(maxConnsPerHost)
	}
	return int(max(concurrency, 4))
}

// newTransport builds the client transport on top of cleanhttp's pooled
// defaults. Building a bare &http.Transport{} (as this code used to) drops
// Proxy, dial keepalives and HTTP/2, and — the expensive part — leaves
// MaxIdleConnsPerHost at Go's default of 2: with a concurrency of 50, all but
// two of the in-flight requests then pay a fresh TCP+TLS handshake on every
// single call, and foreman pays for as many handshakes.
//
// Only the idle pool is sized. MaxConnsPerHost is deliberately left unset: it
// is a hard cap on open connections, and both collectors can fan out at once,
// so capping it at the concurrency would serialise them.
func newTransport(skipTLSVerify bool, idleConnsPerHost int) *http.Transport {
	t := cleanhttp.DefaultPooledTransport()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: skipTLSVerify} // #nosec G402
	if t.MaxIdleConnsPerHost < idleConnsPerHost {
		t.MaxIdleConnsPerHost = idleConnsPerHost
	}
	if t.MaxIdleConns < t.MaxIdleConnsPerHost {
		t.MaxIdleConns = t.MaxIdleConnsPerHost
	}
	return t
}

func NewHTTPClient(cfg ClientConfig) *HTTPClient {
	transport := newTransport(cfg.SkipTLSVerify, resolveIdleConns(cfg.MaxConnsPerHost, cfg.Concurrency))

	// Wrap the default RoundTripper with middleware.
	roundTripper := promhttp.InstrumentRoundTripperInFlight(inFlightGaugeMetric,
		promhttp.InstrumentRoundTripperCounter(counterMetric,
			promhttp.InstrumentRoundTripperDuration(histVecMetric, transport),
		),
	)

	client := retryablehttp.NewClient()
	client.HTTPClient.Transport = roundTripper
	client.RetryMax = int(cfg.RetryMax)
	client.Backoff = retryAfterBackoff(cfg.Log, cfg.RetryMaxWait)

	if cfg.Log == nil {
		client.Logger = nil
	} else {
		client.Logger = &LeveledLogrus{cfg.Log}
	}

	return &HTTPClient{
		client:               client,
		BaseURL:              cfg.BaseURL,
		Username:             cfg.Username,
		Password:             cfg.Password,
		Concurrency:          cfg.Concurrency,
		Limit:                cfg.Limit,
		Search:               cfg.Search,
		SearchHostFact:       cfg.SearchHostFact,
		IncludeHostFactRegex: cfg.IncludeHostFactRegex,
		ExcludeHostFactRegex: cfg.ExcludeHostFactRegex,
		Log:                  cfg.Log,
	}
}

// Set hosts prometheus registry
func (c *HTTPClient) SetHostsRegistry(reg prometheus.Registerer) {
	reg.MustRegister(
		hostsCounterMetric,
		hostsHistVecMetric,
	)
}

// Set hosts facts prometheus registry
func (c *HTTPClient) SetHostsFactsRegistry(reg prometheus.Registerer) {
	reg.MustRegister(
		hostsFactsHistVecMetric,
		hostsFactsCounterMetric,
	)
}

// OnRequestCompleted sets the API request completion callback
func (c *HTTPClient) OnRequestCompleted(rc RequestCompletionCallback) {
	c.onRequestCompleted = rc
}

// DoWithContext sends an API Request and returns back the response. The API response is checked  to see if it was
// a successful call. A successful call is then checked to see if we need to unmarshal since some resources
// have their own implements of unmarshal.
func (c *HTTPClient) DoWithContext(ctx context.Context, r *http.Request, data interface{}) error {
	rreq, err := retryablehttp.FromRequest(r)
	if err != nil {
		return err
	}

	rreq = rreq.WithContext(ctx)

	rreq.Header.Add("User-Agent", UserAgent)

	res, err := c.client.Do(rreq)

	if c.onRequestCompleted != nil {
		c.onRequestCompleted(r, res)
	}

	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if res.StatusCode >= http.StatusOK && res.StatusCode <= http.StatusNoContent {
		if data != nil {
			if err := json.Unmarshal(body, data); err != nil {
				return err
			}
		}
		return nil
	}

	return errors.New(string(body))
}

// logWarn and logInfo go through the optional client logger: Log may be nil,
// and the request error paths used to dereference it and panic.
func (c *HTTPClient) logWarn(msg string, fields logrus.Fields) {
	if c.Log == nil {
		return
	}
	c.Log.WithFields(fields).Warn(msg)
}

func (c *HTTPClient) logInfof(format string, args ...interface{}) {
	if c.Log == nil {
		return
	}
	c.Log.Infof(format, args...)
}

func (c *HTTPClient) GetHosts(ctx context.Context, thin string, page, perPage int64) (HostResponse, error) {
	var result HostResponse

	params := url.Values{}
	params.Set("search", c.Search)

	if thin == "true" {
		params.Set("thin", thin)
	}
	params.Add("page", fmt.Sprintf("%d", page))
	params.Add("per_page", fmt.Sprintf("%d", perPage))

	hostURL, _ := url.ParseRequestURI(c.BaseURL.String())
	hostURL.Path = "api/v2/hosts"
	hostURL.RawQuery = params.Encode()

	req, err := http.NewRequest("GET", hostURL.String(), nil)
	if err != nil {
		return result, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/json")

	err = c.DoWithContext(ctx, req, &result)
	if err != nil {
		hostsCounterMetric.WithLabelValues("failed").Inc()

		var errResult ErrorResult
		if jsonErr := json.Unmarshal([]byte(err.Error()), &errResult); jsonErr != nil {
			c.logWarn("failed to get hosts", logrus.Fields{
				"path": req.URL.Path,
				"err":  err.Error(),
			})
		} else {
			c.logWarn("failed to get hosts", logrus.Fields{
				"path":   req.URL.Path,
				"status": errResult.Status,
				"err":    errResult.Error,
			})
		}
		return result, err
	}
	hostsCounterMetric.WithLabelValues("success").Inc()
	return result, nil
}

func (c *HTTPClient) GetHostFacts(ctx context.Context, hostID, page, perPage int64) (HostFactsResponse, error) {
	var result HostFactsResponse

	params := url.Values{}
	params.Set("search", c.SearchHostFact)
	params.Add("page", fmt.Sprintf("%d", page))
	params.Add("per_page", fmt.Sprintf("%d", perPage))

	facterURL, _ := url.ParseRequestURI(c.BaseURL.String())
	facterURL.Path = fmt.Sprintf("api/v2/hosts/%d/facts", hostID)
	facterURL.RawQuery = params.Encode()

	req, err := http.NewRequest("GET", facterURL.String(), nil)
	if err != nil {
		return result, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/json")

	err = c.DoWithContext(ctx, req, &result)
	if err != nil {
		hostsFactsCounterMetric.WithLabelValues("failed").Inc()

		var errResult ErrorResult
		if jsonErr := json.Unmarshal([]byte(err.Error()), &errResult); jsonErr != nil {
			c.logWarn("failed to get host facts", logrus.Fields{
				"host_id": hostID,
				"path":    req.URL.Path,
				"err":     err.Error(),
			})
		} else {
			c.logWarn("failed to get host facts", logrus.Fields{
				"host_id": hostID,
				"path":    req.URL.Path,
				"status":  errResult.Status,
				"err":     errResult.Error,
			})
		}

		return result, err
	}
	hostsFactsCounterMetric.WithLabelValues("success").Inc()
	return result, nil
}

// GetHostFactsWithConcurrency sends requests in parallel but only up to a certain
// limit, and furthermore it's only parallel up to the amount of CPUs but
// is always concurrent up to the concurrency limit
func (c *HTTPClient) GetHostFactsWithConcurrency(hosts []Host) []HostFactsWithConcurrencyResult {

	// this buffered channel will block at the concurrency limit
	semaphoreChan := make(chan struct{}, c.Concurrency)

	// this channel will not block and collect the http request results
	resultsChan := make(chan *HostFactsWithConcurrencyResult)

	// make sure we close these channels when we're done with them
	defer func() {
		close(semaphoreChan)
		close(resultsChan)
	}()

	if len(hosts) == 0 {
		return nil
	}

	start := time.Now()
	// keen an index and loop through every host we will send a request to
	for i, host := range hosts {

		// start a go routine with the index and hostID in a closure
		go func(i int, hostID int64) {

			// this sends an empty struct into the semaphoreChan which
			// is basically saying add one to the limit, but when the
			// limit has been reached block until there is room
			semaphoreChan <- struct{}{}

			// send the request and put the response in a result struct
			// along with the index so we can sort them later along with
			// any error that might have occured
			ctx := context.Background()
			res, err := c.GetHostFacts(ctx, hostID, int64(1), int64(1000))
			result := &HostFactsWithConcurrencyResult{i, res, err}

			// now we can send the result struct through the resultsChan
			resultsChan <- result

			// once we're done it's we read from the semaphoreChan which
			// has the effect of removing one from the limit and allowing
			// another goroutine to start
			<-semaphoreChan

		}(i, host.ID)
	}

	// make a slice to hold the results we're expecting
	var results []HostFactsWithConcurrencyResult

	// start listening for any results over the resultsChan
	// once we get a result append it to the result slice
	for {
		result := <-resultsChan
		results = append(results, *result)

		// if we've reached the expected amount of hosts then stop
		if len(results) == len(hosts) {
			break
		}
	}
	duration := time.Since(start)
	hostsFactsHistVecMetric.WithLabelValues().Observe(duration.Seconds())

	// let's sort these results real quick
	sort.Slice(results, func(i, j int) bool {
		return results[i].Index < results[j].Index
	})

	// now we're done we return the results
	return results
}

// expectedHosts returns how many hosts foreman says match the search. Subtotal
// is the count for the current search and equals Total when no search is set;
// Total on its own counts every host, so paginating on it makes the exporter
// request empty pages as soon as --search is used. Fall back to Total for
// foreman versions that do not report subtotal.
func expectedHosts(resp HostResponse) int64 {
	if resp.Subtotal != nil {
		return *resp.Subtotal
	}
	return resp.Total
}

// pageCount returns how many pages cover expected hosts at perPage per page,
// clamped by the client --limit.
func (c *HTTPClient) pageCount(expected, perPage int64) int64 {
	if perPage <= 0 || expected <= 0 {
		return 0
	}
	if c.Limit != 0 && c.Limit < expected {
		expected = c.Limit
	}
	return int64(math.Ceil(float64(expected) / float64(perPage)))
}

func (c *HTTPClient) GetHostsFactsFiltered(perPage int64) (map[string]map[string]string, error) {
	if c.Limit != 0 && c.Limit < perPage {
		perPage = int64(c.Limit)
	}

	ctx := context.Background()
	hostsFirstPage, err := c.GetHosts(ctx, "true", 1, perPage)
	if err != nil {
		errMsg := fmt.Errorf("cannot get foreman hosts: %v", err)
		return nil, errMsg
	}
	hosts := hostsFirstPage.Results

	// Page on the echoed per_page: foreman may clamp the requested value.
	effPerPage := hostsFirstPage.PerPage
	if effPerPage <= 0 {
		effPerPage = perPage
	}
	expected := expectedHosts(hostsFirstPage)
	pages := c.pageCount(expected, effPerPage)

	for page := hostsFirstPage.Page + 1; page <= pages; page++ {
		hostsPage, err := c.GetHosts(ctx, "true", page, perPage)
		if err != nil {
			errMsg := fmt.Errorf("cannot get foreman hosts page (%d/%d) %v", page, pages, err)
			return nil, errMsg
		}
		hosts = append(hosts, hostsPage.Results...)
	}

	hostsTotal := len(hosts)
	c.logInfof("found %d hosts", hostsTotal)

	results := c.GetHostFactsWithConcurrency(hosts)

	var errCount int
	hostsFacts := make(map[string]map[string]string, len(results))
	for _, item := range results {
		if item.Error != nil {
			errCount++
			continue
		}

		for name, data := range item.Result.Results {
			factsMap := make(map[string]string)

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

			hostsFacts[name] = factsMap
		}
	}

	if errCount > 0 {
		return hostsFacts, fmt.Errorf("expected '%d' got '%d'", hostsTotal, hostsTotal-errCount)
	}

	return hostsFacts, nil
}

func (c *HTTPClient) GetHostsFiltered(perPage int64) ([]Host, error) {
	if c.Limit != 0 && c.Limit < perPage {
		perPage = int64(c.Limit)
	}

	ctx := context.Background()
	// Only the counters are needed here; the pages themselves are fetched below
	// with thin=false. Asking for a single row keeps this probe cheap instead of
	// downloading a full page that is then thrown away and refetched.
	hostsFirstPage, err := c.GetHosts(ctx, "true", 1, 1)
	if err != nil {
		errMsg := fmt.Errorf("cannot get foreman hosts: %v", err)
		return nil, errMsg
	}

	expected := expectedHosts(hostsFirstPage)
	pages := c.pageCount(expected, perPage)
	if pages == 0 {
		return nil, nil
	}

	pagesSlice := make([]int64, 0, pages)
	for i := int64(1); i <= pages; i++ {
		pagesSlice = append(pagesSlice, i)
	}

	results := c.GetHostWithConcurrency(pagesSlice, perPage)

	var hostResults []Host

	var errCount int
	for _, item := range results {
		if item.Error != nil {
			errCount++
			continue
		}
		hostResults = append(hostResults, item.Result.Results...)
	}

	// Report a short read so the caller can tell a partial list from a complete
	// one. The hosts collected so far are returned either way.
	if c.Limit != 0 && c.Limit < expected {
		expected = c.Limit
	}
	if errCount > 0 || int64(len(hostResults)) < expected {
		return hostResults, fmt.Errorf("incomplete host list: expected %d hosts, got %d (%d/%d pages failed)", expected, len(hostResults), errCount, len(pagesSlice))
	}

	return hostResults, nil
}

// GetHostWithConcurrency sends requests in parallel but only up to a certain
// limit, and furthermore it's only parallel up to the amount of CPUs but
// is always concurrent up to the concurrency limit
func (c *HTTPClient) GetHostWithConcurrency(pages []int64, perPage int64) []HostWithConcurrencyResult {

	// this buffered channel will block at the concurrency limit
	semaphoreChan := make(chan struct{}, c.Concurrency)

	// this channel will not block and collect the http request results
	resultsChan := make(chan *HostWithConcurrencyResult)

	// make sure we close these channels when we're done with them
	defer func() {
		close(semaphoreChan)
		close(resultsChan)
	}()

	if len(pages) == 0 {
		return nil
	}

	start := time.Now()
	// keen an index and loop through every host we will send a request to
	for i, page := range pages {

		// start a go routine with the index and hostID in a closure
		go func(i int, page, perPage int64) {

			// this sends an empty struct into the semaphoreChan which
			// is basically saying add one to the limit, but when the
			// limit has been reached block until there is room
			semaphoreChan <- struct{}{}

			// send the request and put the response in a result struct
			// along with the index so we can sort them later along with
			// any error that might have occured
			ctx := context.Background()
			res, err := c.GetHosts(ctx, "false", page, perPage)
			result := &HostWithConcurrencyResult{i, res, err}

			// now we can send the result struct through the resultsChan
			resultsChan <- result

			// once we're done it's we read from the semaphoreChan which
			// has the effect of removing one from the limit and allowing
			// another goroutine to start
			<-semaphoreChan

		}(i, page, perPage)
	}

	// make a slice to hold the results we're expecting
	var results []HostWithConcurrencyResult

	// start listening for any results over the resultsChan
	// once we get a result append it to the result slice
	for {
		result := <-resultsChan
		results = append(results, *result)

		// if we've reached the expected amount of pages then stop
		if len(results) == len(pages) {
			break
		}
	}
	duration := time.Since(start)
	hostsHistVecMetric.WithLabelValues().Observe(duration.Seconds())

	// let's sort these results real quick
	sort.Slice(results, func(i, j int) bool {
		return results[i].Index < results[j].Index
	})

	// now we're done we return the results
	return results
}
