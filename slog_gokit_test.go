package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

// newTestLogger returns a go-kit logger backed by a slog JSON handler writing to
// buf, at the given minimum level.
func newTestLogger(buf *bytes.Buffer, lvl slog.Level) log.Logger {
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: lvl})
	return newGoKitLogger(slog.New(h))
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	if buf.Len() == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("failed to decode slog output %q: %v", buf.String(), err)
	}
	return m
}

func TestSlogGoKitLevelMapping(t *testing.T) {
	tests := []struct {
		name    string
		emit    func(l log.Logger)
		wantLvl string
	}{
		{"debug", func(l log.Logger) { _ = level.Debug(l).Log("msg", "m") }, "DEBUG"},
		{"info", func(l log.Logger) { _ = level.Info(l).Log("msg", "m") }, "INFO"},
		{"warn", func(l log.Logger) { _ = level.Warn(l).Log("msg", "m") }, "WARN"},
		{"error", func(l log.Logger) { _ = level.Error(l).Log("msg", "m") }, "ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(newTestLogger(&buf, slog.LevelDebug))
			m := decode(t, &buf)
			if m["level"] != tc.wantLvl {
				t.Fatalf("level = %v, want %v (out=%s)", m["level"], tc.wantLvl, buf.String())
			}
			if m["msg"] != "m" {
				t.Fatalf("msg = %v, want %q", m["msg"], "m")
			}
		})
	}
}

func TestSlogGoKitAttributesAndMsg(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, slog.LevelInfo)

	// go-kit convention: level + msg + extra key/values, as dskit emits.
	_ = level.Warn(l).Log("msg", "cache miss", "key", "collectors/host", "count", 3)

	m := decode(t, &buf)
	if m["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", m["level"])
	}
	if m["msg"] != "cache miss" {
		t.Fatalf("msg = %v, want %q", m["msg"], "cache miss")
	}
	if m["key"] != "collectors/host" {
		t.Fatalf("key attr = %v, want %q", m["key"], "collectors/host")
	}
	if m["count"] != float64(3) {
		t.Fatalf("count attr = %v, want 3", m["count"])
	}
}

func TestSlogGoKitNoLevelDefaultsInfo(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, slog.LevelDebug)

	// A bare Log call with no level key should default to info.
	_ = l.Log("msg", "plain", "k", "v")

	m := decode(t, &buf)
	if m["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", m["level"])
	}
	if m["msg"] != "plain" || m["k"] != "v" {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestSlogGoKitWithComponent(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf, slog.LevelInfo)

	// dskit wraps loggers with log.With(logger, "component", "ring").
	_ = level.Info(log.With(l, "component", "ring")).Log("msg", "started")

	m := decode(t, &buf)
	if m["component"] != "ring" {
		t.Fatalf("component attr = %v, want %q", m["component"], "ring")
	}
	if m["msg"] != "started" {
		t.Fatalf("msg = %v, want %q", m["msg"], "started")
	}
}

// ensure the adapter satisfies the go-kit interface and never returns an error.
func TestSlogGoKitLogReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	l := newGoKitLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	if err := l.Log("msg", "x"); err != nil {
		t.Fatalf("Log returned error: %v", err)
	}
}
