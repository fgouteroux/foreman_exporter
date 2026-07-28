## Unreleased

* [BUGFIX] stop zeroing the memberlist TCP transport timeouts: the whole `TCPTransportConfig` was being replaced, leaving `MaxConcurrentWrites`=1 and `AcquireWriterTimeout`=0, which dropped probe ACKs/gossip on high-latency (cross-DC) links and made the cluster flap (`no acks received` / health score pegged). Bind addr/port are now merged into the existing transport config.
* [CHANGE] expose the full dskit memberlist KV config on the CLI under `--ring.memberlist.*` (gossip, probe, push/pull, rejoin, join-backoff, transport timeouts, …) and the ring lifecycler settings `--ring.heartbeat-period`, `--ring.heartbeat-timeout`, `--ring.keep-instance-in-ring-on-shutdown`, instead of hardcoded values; tune them per environment. Note: `stream-timeout` and `rejoin-interval` now default to dskit values (2s and 0) — set `--ring.memberlist.stream-timeout`/`--ring.memberlist.rejoin-interval` (and, for cross-DC, `--ring.memberlist.max-concurrent-writes` / `--ring.memberlist.acquire-writer-timeout`) as needed.

## 1.1.0 / 2026-07-28

* [BUGFIX] stop a goroutine/memory leak by reusing a shared zstd encoder/decoder instead of creating one (never closed) per scrape, which exhausted the process and collapsed the memberlist cluster under load
* [BUGFIX] re-enable memberlist anti-entropy (`PushPullInterval`) and add periodic rejoin so nodes reconverge after a restart or transient network blip instead of staying split
* [BUGFIX] log the resolved cache compression bool instead of the flag pointer
* [FEATURE] add `foreman_exporter_host_scrape_duration_seconds` and `foreman_exporter_host_facts_scrape_duration_seconds` gauges (and log the duration on cache update) to measure how long a full collector scrape takes
* [FEATURE] index page: show the ring members and leader, exporter version, foreman url, and per-collector cache config; add a `/status` JSON endpoint; clearer section names and endpoint paths

## 1.0.0 / 2026-07-28

* [CHANGE] go 1.26 and refresh all Go dependencies (dskit, prometheus/common v0.70, exporter-toolkit v0.17, client_golang v1.23)
* [CHANGE] migrate logging from go-kit/promlog to log/slog (promslog); dskit keeps go-kit via an internal slog adapter so all output stays unified
* [CHANGE] migrate golangci-lint to v2 (config schema and CI action)
* [CHANGE] store the collectors' KV cache as a protobuf value (dskit codec.Proto) instead of a JSON/quoted-string blob, shrinking the gossiped value and dropping the json-iterator dependency
* [ENHANCEMENT] replace golang.org/x/exp/slices with stdlib slices and github.com/pkg/errors with fmt.Errorf
* [FEATURE] honor the Retry-After header (delay-seconds and HTTP-date) when foreman rate-limits the client
* [FEATURE] add `--retry-max` flag to configure the foreman client max retries
* [FEATURE] add `foreman_exporter_client_retry_after_seconds` histogram (labeled by status) for honored Retry-After delays


## 0.0.7 / 2024-04-167

* [FEATURE] go 1.22
* [FEATURE] enable cache for host collector
* [FIX] return error if cache not enabled and params given to the query in host fact collector
* [FIX] remove plural word in endpoint path
* [FIX] scrape error metric name in host collector


## 0.0.6 / 2024-04-16

* [FEATURE] allow disabling cache in uri param for hostfact collector
* [FEATURE] handle scrape timeout and move to dedicated endpoint for host collector
* [FEATURE] handle scrape timeout and expired-cache for hostfact collector
* [FIX] return expired cache on scrape error/timeout only if the param expired-cache is true
* [FIX] enhance foreman client logging and update host fact cache key only if no error


## 0.0.5 / 2024-01-15

* [FEATURE] add flag to filter foreman hosts search


## 0.0.4 / 2024-01-12

* [FEATURE] add user agent http header in foreman requests
* [FEATURE] add flag to lock concurrent requests on collectors


## 0.0.3 / 2023-12-15

* [ENHANCEMENT] logging messages more clear about skipping metrics collection
* [ENHANCEMENT] hostfact collector: use jsonCodec encode func and add dedicated updateKV func
* [FIX] enable hostfact collector caching if flag `--cache.enabled` is given


## 0.0.2 / 2023-12-14

* [FEATURE] add skip-tls-verify flag for foreman HTTP client
* [FIX] name label in hostfact collector (consistency with host collector)

## 0.0.1 / 2023-12-13

* [FEATURE] first version
