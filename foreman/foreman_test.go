package foreman

import (
	"net/http"
	"testing"
	"time"
)

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
