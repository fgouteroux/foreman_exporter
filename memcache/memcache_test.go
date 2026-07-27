package memcache

import (
	"testing"
	"time"
)

func TestMemCacheSetGet(t *testing.T) {
	c := NewLocalCache()

	if _, found := c.Get("missing"); found {
		t.Fatalf("Get on empty cache returned found=true")
	}

	c.Set("k", "v", time.Hour)
	got, found := c.Get("k")
	if !found {
		t.Fatalf("Get after Set returned found=false")
	}
	if got.Value != "v" {
		t.Fatalf("Get returned %v, want %q", got.Value, "v")
	}
	if got.ExpiresAt.Before(time.Now()) {
		t.Fatalf("ExpiresAt %v is already in the past", got.ExpiresAt)
	}
}

func TestMemCacheExpiresAt(t *testing.T) {
	c := NewLocalCache()
	c.Set("k", 42, 50*time.Millisecond)

	got, _ := c.Get("k")
	// The entry is still stored; callers compare ExpiresAt themselves.
	if !got.ExpiresAt.After(time.Now()) {
		t.Fatalf("expected ExpiresAt in the future right after Set")
	}

	deadline := got.ExpiresAt
	time.Sleep(60 * time.Millisecond)
	if time.Now().Before(deadline) {
		t.Fatalf("clock did not advance past the TTL")
	}
	// The value is retained (Get does not evict); expiry is a caller concern.
	if _, found := c.Get("k"); !found {
		t.Fatalf("Get evicted an expired entry; it should be retained")
	}
}

func TestMemCacheDel(t *testing.T) {
	c := NewLocalCache()
	c.Set("k", "v", time.Hour)
	c.Del("k")
	if _, found := c.Get("k"); found {
		t.Fatalf("Get after Del returned found=true")
	}
	// Deleting a missing key is a no-op.
	c.Del("missing")
}

func TestMemCacheOverwrite(t *testing.T) {
	c := NewLocalCache()
	c.Set("k", "old", time.Hour)
	c.Set("k", "new", time.Hour)
	got, _ := c.Get("k")
	if got.Value != "new" {
		t.Fatalf("overwrite failed: got %v, want %q", got.Value, "new")
	}
}
