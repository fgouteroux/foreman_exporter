package main

import (
	"fmt"
	"time"

	jsoniter "github.com/json-iterator/go"

	"github.com/grafana/dskit/kv/memberlist"
)

type Cache struct {
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Merge implements the memberlist.Mergeable interface.
// It allow to merge the content of two different data.
// We dont need to compare values to know if a change is requested as the leader only could send a message
func (c *Cache) Merge(mergeable memberlist.Mergeable, _ bool) (change memberlist.Mergeable, error error) {
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

	if other.CreatedAt.Before(c.CreatedAt) {
		return nil, nil
	}

	if c.CreatedAt == other.CreatedAt {
		return nil, nil
	}

	// request a change.
	*c = *other
	return other, nil
}

// MergeContent tells if the content of the two objects are the same.
func (c *Cache) MergeContent() []string {
	return []string{c.Content}
}

// RemoveTombstones is not required
func (c *Cache) RemoveTombstones(_ time.Time) (total, removed int) {
	return 0, 0
}

func (c *Cache) Clone() memberlist.Mergeable {
	clone := *c
	return &clone
}

var JSONCodec = jsonCodec{}

type jsonCodec struct{}

func (jsonCodec) Decode(data []byte) (interface{}, error) {
	var value Cache
	if err := jsoniter.ConfigFastest.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (jsonCodec) Encode(obj interface{}) ([]byte, error) {
	return jsoniter.ConfigFastest.Marshal(obj)
}
func (jsonCodec) CodecID() string { return "jsonCodec" }
