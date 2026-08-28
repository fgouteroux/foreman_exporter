## 1.4.0 / 2026-08-28

**Upgrade note.** The host fact collector now uses `/api/v2/fact_values` by default. Both routes are the same controller action in foreman, so the facts returned are identical, but the query uses scoped_search's `^` (in) operator to select host ids — which belongs to foreman's search layer rather than its documented API surface. If your foreman rejects it the collector will return `400`s right after the upgrade, with no configuration change on your side. Two ways back: `--collector.hostfact.in-operator=or` keeps the bulk mode with a longer but more conservative query, and `--no-collector.hostfact.bulk` restores the previous per-host behaviour entirely.

* [FEATURE] collect host facts in bulk through `/api/v2/fact_values` (`--collector.hostfact.bulk`, on by default), one request per batch of host ids instead of one request per host. Both routes are the same controller action in foreman — the per-host one only appends `host = <id>` to the search — so this is pure batching, not a change of semantics. On a fleet of a few thousand hosts it turns thousands of requests per pass into a few dozen
* [FEATURE] add `--collector.hostfact.names` to push the fact-name selection server-side instead of downloading every fact and discarding most of them client-side. It composes with `--collector.hostfact.include` as an intersection, so the list must be a superset of what the regex keeps; empty (the default) means no server-side name filter
* [FEATURE] add `--collector.hostfact.batch-size`, `--collector.hostfact.per-page`, `--collector.hostfact.max-pages`, `--collector.hostfact.in-operator` and `--collector.hostfact.max-url-length` to size the batches. A response that fills a page is an order of magnitude slower than one that does not, so the batch size is bounded by the fact rows it brings back, not by the host count
* [FEATURE] add `foreman_exporter_client_fact_values_requests_total{status}` and `..._fact_values_request_duration_seconds{status}`, plus `foreman_exporter_host_facts_batches_total`, `..._batches_failed_total`, `..._batches_maxpages_total`, `..._hosts_lost_total`, `..._hosts_without_facts`, `..._pages_continued_total`, `..._per_page_clamped_total`, `..._page_rows`, `..._page_fill_ratio` and `..._batch_hosts`. The two to alert on are `batches_failed_total` and `batches_maxpages_total`, as a ratio over `batches_total`
* [ENHANCEMENT] a batch that exhausts `--collector.hostfact.max-pages` returned incomplete facts and is reported as an error, so it neither shrinks the exported labels unnoticed nor refreshes the cache; counted by `foreman_exporter_hostfact_batches_maxpages_total`. A correctly sized batch fits in a single page, so hitting the limit means the sizing is wrong
* [ENHANCEMENT] a failed or truncated batch drops all of its hosts rather than export them with an amputated set of facts: facts are const labels, so a shorter label set is a different time series, not a smaller one. The other batches are still returned, with an error naming how many hosts were lost. Pagination inside a batch merges pages per host, since foreman paginates on fact rows and a host can straddle a page boundary
* [BUGFIX] stop logging `cache is empty` when the cache was never read. With `?cache=false` the collector bypasses the cache on purpose, so it has no grounds to say anything about its contents; the message now states the actual reason there is nothing to export (scrape failed and cache bypassed, disabled, or genuinely empty)
* [ENHANCEMENT] batches are shrunk automatically when their encoded search would exceed the server request-line limit, which would otherwise surface as an opaque 414

## 1.3.0 / 2026-08-26

* [BUGFIX] export partial collector results instead of dropping them: both collectors put the whole result handling in the `else` of the error check, so a single rate-limited host (or one failed page) discarded an entire multi-minute scrape and left no series at all. Partial results are now exported, and the cache is only refreshed from them when `--collector.hostfact.cache.update-on-partial` is set
* [BUGFIX] stop the empty scrape result from clobbering the cached value: the goroutine pushed a nil slice over the result channel, overwriting the (expired) cache that had just been loaded, which made `?expired-cache=true` a no-op on the error path
* [BUGFIX] apply `--collector.host.labels-include` / `--collector.host.labels-exclude`: the filtered label set was built and then thrown away, the unfiltered one being exported. **The series exported by the host collector change if you use these flags**
* [BUGFIX] paginate the host list on `subtotal` (how many hosts match the search) instead of `total` (how many hosts exist): with a `--search` set the page count was overestimated and the exporter fetched empty pages. Falls back to `total` when foreman does not report `subtotal`
* [BUGFIX] return an empty result instead of blocking forever when the search matches no host: the page count was 0 and the collection loop waited on a result that was never produced
* [BUGFIX] stop panicking on a request error when the foreman client is built without a logger
* [BUGFIX] report a meaningful error on a short host list (`incomplete host list: expected N hosts, got M (X/Y pages failed)`); the previous message compared a page size to a page-error count
* [FEATURE] add `foreman_exporter_node_role` (0 = unknown, 1 = leader, 2 = follower), resolved at scrape time, so the leader is identifiable from any replica: `count(foreman_exporter_node_role == 1)` alerts on `0` (no leader) and `> 1` (split ring). A ring that cannot be resolved reports `unknown` instead of defaulting to follower, and increments `foreman_exporter_ring_leader_lookup_errors_total`
* [ENHANCEMENT] build the foreman client transport from `cleanhttp`'s pooled defaults instead of a bare `http.Transport`, and size the idle connection pool on the concurrency (`--foreman.max-conns-per-host`). The bare transport left `MaxIdleConnsPerHost` at Go's default of 2, so all but two of the in-flight requests paid a full TCP+TLS handshake on every call; it also dropped proxy support and HTTP/2
* [FEATURE] add `--foreman.rate-limit` (requests per second) and `--foreman.rate-limit-burst` to pace outgoing requests client-side. The limiter sits at the `http.RoundTripper` level so it covers retryablehttp's retries too, and wraps the instrumentation so the time spent waiting is excluded from `foreman_exporter_client_request_duration_seconds` and from the in-flight gauge. New metrics: `foreman_exporter_client_rate_limit_wait_seconds_total`, `foreman_exporter_client_rate_limit_delayed_requests_total`, `foreman_exporter_client_rate_limit_requests_per_second`. Rate limiting bounds throughput where `--concurrency` bounds parallelism; both matter, and which one binds depends on how fast foreman answers
* [ENHANCEMENT] cap the honored `Retry-After` delay with `--foreman.retry-max-wait` (default 60s), so a long rate-limit response no longer parks a worker on an idle concurrency slot
* [ENHANCEMENT] only fetch the counters (`per_page=1`) when probing the host list, instead of downloading a full page that was immediately refetched
* [ENHANCEMENT] update `*_scrape_duration_seconds` on every outcome, so a slow-and-failing scrape is measured too
* [ENHANCEMENT] build the fact-name label sanitiser once instead of once per fact (it was allocated in the innermost hosts x facts loop on every scrape)
* [CHANGE] `foreman.NewHTTPClient` now takes a `foreman.ClientConfig` struct instead of 12 positional arguments
* [FIX] allocate the local cache when `--cache.enabled` is set without the ring and without a per-collector cache flag; it was left nil and dereferenced on the first scrape

## 1.2.1 / 2026-08-25

* [BUGFIX] log the actual foreman error when a request fails: the inner `json.Unmarshal` of the error body shadowed the outer `err`, so failures that don't carry a JSON body (client retries exhausted, transport errors) were reported as `invalid character 'X' looking for beginning of value` instead of the real cause

## 1.2.0 / 2026-07-28

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
