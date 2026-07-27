package main

import (
	"context"
	"log/slog"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

// slogGoKit adapts a *slog.Logger to the go-kit log.Logger interface. dskit
// (ring, memberlist, kv) and its lifecycler still log through go-kit; wrapping
// the exporter's slog logger lets that output flow through the same slog handler
// as the rest of the process instead of a second, differently-formatted stream.
type slogGoKit struct {
	logger *slog.Logger
}

// newGoKitLogger returns a go-kit log.Logger backed by the given slog logger.
func newGoKitLogger(logger *slog.Logger) log.Logger {
	return slogGoKit{logger: logger}
}

// Log implements log.Logger. It extracts the go-kit level and "msg" keys and
// forwards the remaining key/value pairs to slog as attributes.
func (s slogGoKit) Log(keyvals ...interface{}) error {
	lvl := slog.LevelInfo
	msg := ""
	attrs := make([]interface{}, 0, len(keyvals))

	for i := 0; i+1 < len(keyvals); i += 2 {
		key := keyvals[i]
		val := keyvals[i+1]

		if key == level.Key() {
			lvl = gokitLevel(val)
			continue
		}
		if k, ok := key.(string); ok && k == "msg" {
			if m, ok := val.(string); ok {
				msg = m
				continue
			}
		}
		attrs = append(attrs, key, val)
	}

	s.logger.Log(context.Background(), lvl, msg, attrs...)
	return nil
}

// gokitLevel maps a go-kit level value to its slog equivalent, defaulting to
// info for anything unrecognized.
func gokitLevel(val interface{}) slog.Level {
	v, ok := val.(level.Value)
	if !ok {
		return slog.LevelInfo
	}
	switch v.String() {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
