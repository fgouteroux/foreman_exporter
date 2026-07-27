package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestCacheCodecRoundTrip(t *testing.T) {
	now := time.Now()
	in := &Cache{
		Content:   []byte{0x00, 0x01, 0x02, 0xff, 0xfe}, // arbitrary binary, not valid UTF-8
		CreatedAt: now.UnixNano(),
		ExpiresAt: now.Add(time.Hour).UnixNano(),
	}

	encoded, err := cacheCodec.Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	out, err := cacheCodec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := out.(*Cache)
	if !ok {
		t.Fatalf("Decode returned %T, want *Cache", out)
	}
	if string(got.Content) != string(in.Content) {
		t.Fatalf("Content = %v, want %v", got.Content, in.Content)
	}
	if got.CreatedAt != in.CreatedAt || got.ExpiresAt != in.ExpiresAt {
		t.Fatalf("timestamps = %d/%d, want %d/%d", got.CreatedAt, got.ExpiresAt, in.CreatedAt, in.ExpiresAt)
	}
}

func TestCacheMerge(t *testing.T) {
	base := &Cache{Content: []byte("old"), CreatedAt: 100}

	// newer wins
	c := &Cache{Content: []byte("old"), CreatedAt: 100}
	change, err := c.Merge(&Cache{Content: []byte("new"), CreatedAt: 200}, false)
	if err != nil {
		t.Fatalf("Merge newer: %v", err)
	}
	if change == nil {
		t.Fatal("newer update rejected, want accepted")
	}
	if string(c.Content) != "new" || c.CreatedAt != 200 {
		t.Fatalf("after merge = %q/%d, want new/200", c.Content, c.CreatedAt)
	}

	// older rejected
	c = &Cache{Content: []byte("keep"), CreatedAt: 200}
	change, _ = c.Merge(&Cache{Content: []byte("stale"), CreatedAt: 100}, false)
	if change != nil {
		t.Fatal("older update accepted, want rejected")
	}
	if string(c.Content) != "keep" {
		t.Fatalf("older merge mutated content: %q", c.Content)
	}

	// equal timestamp rejected (avoids gossip loops)
	c = &Cache{Content: []byte("keep"), CreatedAt: 200}
	change, _ = c.Merge(&Cache{Content: []byte("same"), CreatedAt: 200}, false)
	if change != nil {
		t.Fatal("equal-timestamp update accepted, want rejected")
	}

	// nil is a no-op
	if change, err := base.Merge(nil, false); change != nil || err != nil {
		t.Fatalf("Merge(nil) = %v, %v; want nil, nil", change, err)
	}
}

// TestCacheFullPath exercises the exact collector pipeline: host facts →
// json.Marshal → zstd → Cache → codec Encode → Decode → zstd → json.Unmarshal.
func TestCacheFullPath(t *testing.T) {
	hostsData := []map[string]string{
		{"name": "web-01", "os": "CentOS", "cpus": "4"},
		{"name": "web-02", "os": "Debian", "cpus": "8"},
	}

	raw, err := json.Marshal(hostsData)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	enc, _ := zstd.NewWriter(nil)
	compressed := enc.EncodeAll(raw, make([]byte, 0, len(raw)))

	now := time.Now()
	val, err := cacheCodec.Encode(&Cache{
		Content:   compressed,
		CreatedAt: now.UnixNano(),
		ExpiresAt: now.Add(time.Hour).UnixNano(),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	out, err := cacheCodec.Decode(val)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cc := out.(*Cache)

	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	decompressed, err := dec.DecodeAll(cc.Content, make([]byte, 0, len(cc.Content)))
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}

	var got []map[string]string
	if err := json.Unmarshal(decompressed, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 || got[0]["name"] != "web-01" || got[1]["os"] != "Debian" {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}
