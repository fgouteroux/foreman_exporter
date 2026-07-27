package main

import (
	"fmt"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/dskit/kv/codec"
	"github.com/grafana/dskit/kv/memberlist"
)

// cacheCodec serializes Cache values for the collectors' memberlist KV keys.
// It uses gogo protobuf with snappy framing (dskit's codec.Proto), matching the
// codec the ring itself uses, and stores the compressed payload as raw bytes
// rather than a base64/quoted JSON string.
var cacheCodec = codec.NewProtoCodec("cacheProtoCodec", func() proto.Message { return &Cache{} })

// Merge implements memberlist.Mergeable. A single writer (the ring leader) owns
// each key, so there is never a genuine multi-writer conflict to reconcile;
// Merge only decides whether the incoming value is newer than the local one
// (last-writer-wins by CreatedAt) and replaces the whole value if so.
func (c *Cache) Merge(mergeable memberlist.Mergeable, _ bool) (memberlist.Mergeable, error) {
	if mergeable == nil {
		return nil, nil
	}
	other, ok := mergeable.(*Cache)
	if !ok {
		return nil, fmt.Errorf("expected *Cache, got %T", mergeable)
	}
	if other == nil {
		return nil, nil
	}

	// Reject older or identical updates (equal timestamps avoid gossip loops).
	if other.CreatedAt < c.CreatedAt {
		return nil, nil
	}
	if other.CreatedAt == c.CreatedAt {
		return nil, nil
	}

	// request a change.
	*c = *other
	return other, nil
}

// MergeContent tells dskit which content this value carries (diagnostics only).
func (c *Cache) MergeContent() []string {
	return []string{string(c.Content)}
}

// RemoveTombstones is not required: the leader overwrites the key in place.
func (c *Cache) RemoveTombstones(_ time.Time) (total, removed int) {
	return 0, 0
}

func (c *Cache) Clone() memberlist.Mergeable {
	clone := *c
	return &clone
}
