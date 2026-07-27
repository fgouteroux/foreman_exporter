package foreman

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// newTestClient builds an HTTPClient pointed at ts with no retries and no logger.
func newTestClient(t *testing.T, ts *httptest.Server, search string) *HTTPClient {
	t.Helper()
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	return NewHTTPClient(base, "user", "pass", false, 1, 0, 0, search, "", nil, nil, nil)
}

func TestDoWithContextSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"total":2,"page":1,"per_page":20}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "")
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)

	var got HostResponse
	if err := c.DoWithContext(context.Background(), req, &got); err != nil {
		t.Fatalf("DoWithContext returned error: %v", err)
	}
	if got.Total != 2 || got.Page != 1 || got.PerPage != 20 {
		t.Fatalf("unexpected decode: %+v", got)
	}
}

func TestDoWithContextErrorBodyReturned(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "")
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)

	err := c.DoWithContext(context.Background(), req, nil)
	if err == nil {
		t.Fatal("DoWithContext returned nil error on 404")
	}
	if err.Error() != `{"error":"boom"}` {
		t.Fatalf("error body = %q, want the raw response body", err.Error())
	}
}

func TestGetHostsBuildsRequest(t *testing.T) {
	var gotPath, gotSearch, gotPage, gotPerPage, gotThin, gotUser, gotPass string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		q := r.URL.Query()
		gotSearch = q.Get("search")
		gotPage = q.Get("page")
		gotPerPage = q.Get("per_page")
		gotThin = q.Get("thin")
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`{"total":1,"page":3,"per_page":50}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "os_title = CentOS")
	res, err := c.GetHosts(context.Background(), "true", 3, 50)
	if err != nil {
		t.Fatalf("GetHosts returned error: %v", err)
	}

	if gotPath != "/api/v2/hosts" {
		t.Fatalf("path = %q, want /api/v2/hosts", gotPath)
	}
	if gotSearch != "os_title = CentOS" {
		t.Fatalf("search = %q, want the configured filter", gotSearch)
	}
	if gotPage != "3" || gotPerPage != "50" {
		t.Fatalf("page/per_page = %q/%q, want 3/50", gotPage, gotPerPage)
	}
	if gotThin != "true" {
		t.Fatalf("thin = %q, want true", gotThin)
	}
	if gotUser != "user" || gotPass != "pass" {
		t.Fatalf("basic auth = %q/%q, want user/pass", gotUser, gotPass)
	}
	if res.Page != 3 || res.PerPage != 50 || res.Total != 1 {
		t.Fatalf("unexpected decoded response: %+v", res)
	}
}

func TestGetHostsOmitsThinWhenNotTrue(t *testing.T) {
	var hadThin bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadThin = r.URL.Query()["thin"]
		_, _ = w.Write([]byte(`{"total":0,"page":1,"per_page":20}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "")
	if _, err := c.GetHosts(context.Background(), "false", 1, 20); err != nil {
		t.Fatalf("GetHosts returned error: %v", err)
	}
	if hadThin {
		t.Fatal("thin query param present when thin != \"true\"")
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantOK   bool
		wantWait time.Duration
	}{
		{name: "empty", value: "", wantOK: false},
		{name: "delay seconds", value: "120", wantOK: true, wantWait: 120 * time.Second},
		{name: "zero seconds", value: "0", wantOK: true, wantWait: 0},
		{name: "negative seconds", value: "-1", wantOK: false},
		{name: "not a number", value: "soon", wantOK: false},
		{name: "http-date in the past", value: "Wed, 21 Oct 2015 07:28:00 GMT", wantOK: true, wantWait: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value)
			if ok != tc.wantOK {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", tc.value, ok, tc.wantOK)
			}
			if ok && got != tc.wantWait {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.wantWait)
			}
		})
	}
}

func TestParseRetryAfterHTTPDateFuture(t *testing.T) {
	future := time.Now().Add(1 * time.Hour).UTC().Format(http.TimeFormat)
	got, ok := parseRetryAfter(future)
	if !ok {
		t.Fatalf("parseRetryAfter(%q) ok = false, want true", future)
	}
	if got <= 0 || got > time.Hour {
		t.Fatalf("parseRetryAfter(%q) = %v, want a positive duration up to ~1h", future, got)
	}
}
