# foreman_exporter

## Foreman Prometheus Exporter

This [Prometheus](https://prometheus.io/)
[exporter](https://prometheus.io/docs/instrumenting/exporters/)
exposes [foreman](https://www.theforeman.org/) metrics.

![Foreman Exporter](img/home.png)

### Usage

```
usage: foreman_exporter --url=URL --username=USERNAME --password=PASSWORD [<flags>]


Flags:
  -h, --[no-]help                Show context-sensitive help (also try --help-long and --help-man).
      --[no-]web.disable-exporter-metrics  
                                 Exclude metrics about the exporter itself (process_*, go_*).
      --web.telemetry-path="/metrics"  
                                 Path under which to expose metrics.
      --web.prefix-path=""       Prefix path for all http requests.
      --[no-]web.systemd-socket  Use systemd socket activation listeners instead of port listeners
                                 (Linux only).
      --web.listen-address=:11111 ...  
                                 Addresses on which to expose metrics and web interface. Repeatable for
                                 multiple addresses.
      --web.config.file=""       [EXPERIMENTAL] Path to configuration file
                                 that can enable TLS or authentication. See:
                                 https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md
      --url=URL                  Foreman url ($FOREMAN_URL)
      --username=USERNAME        Foreman username ($FOREMAN_USERNAME)
      --password=PASSWORD        Foreman password ($FOREMAN_PASSWORD)
      --[no-]skip-tls-verify     Foreman skip TLS verify ($FOREMAN_SKIP_TLS_VERIFY)
      --concurrency=4            Max concurrent http request
      --limit=0                  Foreman host limit search
      --search=""                Foreman host search filter
      --timeout-offset=0.5       Offset to subtract from Prometheus-supplied timeout in seconds.
      --[no-]collector.lock-concurrent-requests  
                                 Lock concurrent requests on collectors
      --collector=host ...       collector to enabled (repeatable), choices: [host, hostfact]
      --collector.host.labels-include=COLLECTOR.HOST.LABELS-INCLUDE  
                                 host labels to include (regex)
      --collector.host.labels-exclude=COLLECTOR.HOST.LABELS-EXCLUDE  
                                 host labels to exclude (regex)
      --collector.host.timeout=30  
                                 host timeout
      --collector.hostfact.search=COLLECTOR.HOSTFACT.SEARCH  
                                 search host fact query filter
      --collector.hostfact.include=COLLECTOR.HOSTFACT.INCLUDE  
                                 host fact to include (regex)
      --collector.hostfact.exclude=COLLECTOR.HOSTFACT.EXCLUDE  
                                 host fact to exclude (regex)
      --collector.hostfact.timeout=30  
                                 host fact timeout
      --[no-]cache.enabled       Enable cache
      --cache.ttl-expires=1h     Cache Expiration time
      --[no-]cache.compression   Enable zstd compression for kvstore values
      --[no-]ring.enabled        Enable the ring to deduplicate exported foreman metrics.
      --ring.instance-id=RING.INSTANCE-ID  
                                 Instance ID to register in the ring.
      --ring.instance-addr=RING.INSTANCE-ADDR  
                                 IP address to advertise in the ring. Default is auto-detected.
      --ring.instance-port=7946  Port to advertise in the ring.
      --ring.instance-interface-names=RING.INSTANCE-INTERFACE-NAMES  
                                 List of network interface names to look up when finding the instance IP
                                 address.
      --ring.join-members=RING.JOIN-MEMBERS  
                                 Other cluster members to join.
      --log.level=info           Only log messages with the given severity or above. One of: [debug,
                                 info, warn, error]
      --log.format=logfmt        Output format of log messages. One of: [logfmt, json]
      --[no-]version             Show application version.
```

### Metrics Exposed

**Exporter metrics**

This endpoint return metrics about exporter itself and foreman client requests.

```
# HELP foreman_exporter_build_info A metric with a constant '1' value labeled by version, revision, branch, goversion from which foreman_exporter was built, and the goos and goarch for the build.
# TYPE foreman_exporter_build_info gauge
foreman_exporter_build_info{branch="feat/handle_scrape_timeout",goarch="amd64",goos="linux",goversion="go1.21.1",revision="7059cdd4062a29a53cc43225c23061c3b9750aac",tags="unknown",version="0.0.5-2-g7059cdd-dirty"} 1
# HELP foreman_exporter_client_in_flight_requests A gauge of all in-flight requests for the foreman client.
# TYPE foreman_exporter_client_in_flight_requests gauge
foreman_exporter_client_in_flight_requests 0
# HELP foreman_exporter_client_request_duration_seconds A histogram of all request latencies from the foreman client.
# TYPE foreman_exporter_client_request_duration_seconds histogram
foreman_exporter_client_request_duration_seconds_bucket{le="0.005"} 0
foreman_exporter_client_request_duration_seconds_bucket{le="0.01"} 0
foreman_exporter_client_request_duration_seconds_bucket{le="0.025"} 0
foreman_exporter_client_request_duration_seconds_bucket{le="0.05"} 0
foreman_exporter_client_request_duration_seconds_bucket{le="0.1"} 0
foreman_exporter_client_request_duration_seconds_bucket{le="0.25"} 14
foreman_exporter_client_request_duration_seconds_bucket{le="0.5"} 72
foreman_exporter_client_request_duration_seconds_bucket{le="1"} 73
foreman_exporter_client_request_duration_seconds_bucket{le="2.5"} 74
foreman_exporter_client_request_duration_seconds_bucket{le="5"} 74
foreman_exporter_client_request_duration_seconds_bucket{le="10"} 74
foreman_exporter_client_request_duration_seconds_bucket{le="+Inf"} 74
foreman_exporter_client_request_duration_seconds_sum 26.012748153
foreman_exporter_client_request_duration_seconds_count 74
# HELP foreman_exporter_client_requests_total A counter for all requests from the foreman client.
# TYPE foreman_exporter_client_requests_total counter
foreman_exporter_client_requests_total{code="200",method="get"} 74
```

**Foreman hosts status**

Enabled by default.

This collector return metrics to a dedicated endpoint `/host-metrics`.

```
# HELP foreman_exporter_host_status_info Foreman host status
# TYPE foreman_exporter_host_status_info gauge
foreman_exporter_host_status_info{build_status="Installed",configuration_status="Active",global_status="OK",name="server.example.com",organization="example"} 1
```

**Foreman hosts facts**

Enable this collector with the flag `--collector=hostfact`.

This collector return metrics to a dedicated endpoint `/host-facts-metrics`.

Foreman hosts facts could render big metrics labels and must be used with the following flags to reduce the number of labels (labels cardinality):
- `--collector.hostfact.search=`: a foreman query to filter http facts response
- `--collector.hostfact.include=`: a regex to filter facts to include as labels
- `--collector.hostfact.exclude=`: a regex to filter facts to exclude as labels

As foreman host facts collector metrics could return many metrics (depending of foreman hosts number) and labels doesn't change a lot, a memory cache could be enabled.

```
# HELP foreman_exporter_host_facts_info Foreman host facts
# TYPE foreman_exporter_host_facts_info gauge
foreman_exporter_host_facts_info{name="server.example.com", operatingsystem="RedHat",operatingsystemmajrelease="9",operatingsystemrelease="9.2"} 1
```

If the memory cache is enabled and the cache has expired it is possible to use it even if foreman api is not available (network outage, service restart, slow response...). This could prevent hole in metrics scrapping and alerts flapping. To use it, just pass the uri param `expired-cache=true` in scrape config or curl cmd.

```
curl http://localhost:11111/host-facts-metrics?expired-cache=true
```

If the memory cache is enabled, it is possible to force cache regeneration with the param `cache=false`.

```
curl http://localhost:11111/host-facts-metrics?cache=false
```




The following metrics have been added:
```
# HELP foreman_exporter_host_facts_scrape_timeout 1 if timeout occurs, 0 otherwise
# TYPE foreman_exporter_host_facts_scrape_timeout gauge
foreman_exporter_host_facts_scrape_timeout 1
# HELP foreman_exporter_host_facts_use_expired_cache 1 if using expired cache, 0 otherwise
# TYPE foreman_exporter_host_facts_use_expired_cache gauge
foreman_exporter_host_facts_use_expired_cache 1
```

### HA with memberlist

This exporter could be run in cluster mode with memberlist.

![Ring](img/ring.png)

To enable cluster mode, use the following flags:
```
      --ring.instance-id=RING.INSTANCE-ID  
                                 Instance ID to register in the ring.
      --ring.instance-addr=RING.INSTANCE-ADDR  
                                 IP address to advertise in the ring. Default is auto-detected.
      --ring.instance-port=7946  Port to advertise in the ring.
      --ring.instance-interface-names=RING.INSTANCE-INTERFACE-NAMES  
                                 List of network interface names to look up when finding the instance IP address.
      --ring.join-members=RING.JOIN-MEMBERS  
                                 Other cluster members to join.
```

One instance of the ring is elected to be the leader and this is the only one which will make request to foreman and export metrics.

If the leader instance goes down, another one will be elected and will start to export metrics.

![Memberlist](img/memberlist.png)

With this config, it is easy to configure a prometheus agent to scrape the exporter metrics and avoid duplication.

If the foreman host facts collector metrics is enabled with the cache option, the cache is stored in the memberlist kvstore and replicated to all ring instances.

```
      --[no-]cache.enabled       Enable cache
      --cache.ttl-expires=1h     Cache Expiration time
      --[no-]cache.compression   Enable zstd compression for kvstore values
```

### TLS and basic authentication

Foreman Exporter supports TLS and basic authentication. This enables better control of the various HTTP endpoints.

To use TLS and/or basic authentication, you need to pass a configuration file using the `--web.config.file` parameter. The format of the file is described
[in the exporter-toolkit repository](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md).

### Sources

- [Foreman api](https://apidocs.theforeman.org/foreman/2.4/apidoc/v2.html)
- [Hashicorp Memberlist](https://github.com/hashicorp/memberlist)
- [Grafana Distributed systems kit](https://github.com/grafana/dskit)
- [Grafana Mimir Override exporter](https://github.com/grafana/mimir/tree/main/pkg/util/validation/exporter)
