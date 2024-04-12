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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"

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
			Name: "foreman_exporter_client_api_requests_total",
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
	UserAgent = fmt.Sprintf("foreman_exporter/%s", version.Version)
)

func init() {
	// Register all of the metrics in the standard registry.
	prometheus.MustRegister(
		counterMetric,
		histVecMetric,
		inFlightGaugeMetric,
		hostsCounterMetric,
		hostsHistVecMetric,
	)
}

type HostResponse struct {
	Total   int64  `json:"total"`
	Page    int64  `json:"page"`
	PerPage int64  `json:"per_page"`
	Results []Host `json:"results"`
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

func NewHTTPClient(baseURL *url.URL, username, password string, skipTLSVerify bool, concurrency, limit int64, search, searchHostFact string, includeHostFactRegex, excludeHostFactRegex *regexp.Regexp, log *logrus.Logger) *HTTPClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLSVerify}, // #nosec G402
	}

	// Wrap the default RoundTripper with middleware.
	roundTripper := promhttp.InstrumentRoundTripperInFlight(inFlightGaugeMetric,
		promhttp.InstrumentRoundTripperCounter(counterMetric,
			promhttp.InstrumentRoundTripperDuration(histVecMetric, transport),
		),
	)

	client := retryablehttp.NewClient()
	client.HTTPClient.Transport = roundTripper
	client.RetryMax = 3

	if log == nil {
		client.Logger = nil
	} else {
		client.Logger = &LeveledLogrus{log}
	}

	return &HTTPClient{
		client:               client,
		BaseURL:              baseURL,
		Username:             username,
		Password:             password,
		Concurrency:          concurrency,
		Limit:                limit,
		Search:               search,
		SearchHostFact:       searchHostFact,
		IncludeHostFactRegex: includeHostFactRegex,
		ExcludeHostFactRegex: excludeHostFactRegex,
		Log:                  log,
	}
}

// Set prometheus registry
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

	defer res.Body.Close()

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

	start := time.Now()
	// keen an index and loop through every host we will send a request to
	for i, host := range hosts {

		// start a go routine with the index and hostID in a closure
		go func(i int, client *HTTPClient, hostID int64) {

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

		}(i, c, host.ID)
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

	var pages int64
	if c.Limit != 0 && c.Limit <= perPage {
		pages = 1
	} else if c.Limit > perPage {
		pages = int64(math.Ceil(float64(c.Limit) / float64(perPage)))
	} else {
		pages = int64(math.Ceil(float64(hostsFirstPage.Total) / float64(hostsFirstPage.PerPage)))
	}

	for page := hostsFirstPage.Page + 1; page <= pages; page++ {
		hostsPage, err := c.GetHosts(ctx, "true", page, perPage)
		if err != nil {
			errMsg := fmt.Errorf("cannot get foreman hosts page (%d/%d) %v", page, pages, err)
			return nil, errMsg
		}
		hosts = append(hosts, hostsPage.Results...)
	}
	//c.Log.Debugf("Found %d hosts", len(hosts))

	results := c.GetHostFactsWithConcurrency(hosts)

	hostsFacts := make(map[string]map[string]string, len(results))
	for _, item := range results {
		if item.Error != nil {
			c.Log.Errorf("an error occured: %v", err)
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
	return hostsFacts, nil
}

func (c *HTTPClient) GetHostsFiltered(perPage int64) ([]Host, error) {
	if c.Limit != 0 && c.Limit < perPage {
		perPage = int64(c.Limit)
	}

	ctx := context.Background()
	hostsFirstPage, err := c.GetHosts(ctx, "true", 1, perPage)
	if err != nil {
		errMsg := fmt.Errorf("cannot get foreman hosts: %v", err)
		return nil, errMsg
	}

	var pages int64
	if c.Limit != 0 && c.Limit <= perPage {
		pages = 1
	} else if c.Limit > perPage {
		pages = int64(math.Ceil(float64(c.Limit) / float64(perPage)))
	} else {
		pages = int64(math.Ceil(float64(hostsFirstPage.Total) / float64(hostsFirstPage.PerPage)))
	}

	var pagesSlice []int64
	for i := int64(1); i <= pages; i++ {
		pagesSlice = append(pagesSlice, i)
	}

	results := c.GetHostWithConcurrency(pagesSlice, perPage)

	var hostResults []Host
	for _, item := range results {
		if item.Error != nil {
			c.Log.Errorf("an error occured: %v", err)
			continue
		}

		hostResults = append(hostResults, item.Result.Results...)
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

	start := time.Now()
	// keen an index and loop through every host we will send a request to
	for i, page := range pages {

		// start a go routine with the index and hostID in a closure
		go func(i int, client *HTTPClient, page, perPage int64) {

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

		}(i, c, page, perPage)
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
