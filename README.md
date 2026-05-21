# Zabbix NSX-T monitoring

VMware NSX-T 4.2 monitoring with a Prometheus exporter and a Zabbix 7.0
template, designed around nine concrete operational scenarios that an
NSX-T operator wants to be alerted on.

This is a fork of [sapcc/nsx-t-exporter](https://github.com/sapcc/nsx-t-exporter)
extended with collectors that the upstream is missing: edge node runtime
stats, Geneve/TEP tunnels, BGP neighbors, IP pool exhaustion, certificate
expiry, backup status, and a full load-balancer hierarchy (service / virtual
server / pool / pool member).

## Operational scenarios covered

| # | Scenario | Metric / trigger |
|---|---|---|
| 1 | Manager cluster down/degraded | `nsxv3_cluster_management_status` |
| 2 | Edge node down | `nsxv3_edge_node_*` (no-data trigger) |
| 3 | Transport node disconnected | `nsxv3_transport_node_state` |
| 4 | High CPU/Mem on Edge/Manager | `nsxv3_{management,edge}_node_cpu_usage_ratio`, `_memory_usage_ratio` |
| 5 | Geneve/TEP tunnel down | `nsxv3_tunnel_status` |
| 6 | BGP neighbor down | `nsxv3_bgp_neighbor_status` |
| 7 | IP/TEP pool exhaustion | `nsxv3_ip_pool_usage_ratio` |
| 8 | Certificate expiration | `nsxv3_certificate_seconds_to_expiry` |
| 9 | Backup failure | `nsxv3_backup_last_success_timestamp`, `_consecutive_failures` |

Bonus: full load balancer coverage (status + counters at service, virtual
server, pool, and pool-member levels). See [docs/METRICS.md](docs/METRICS.md).

## How it fits together

```
NSX-T 4.2 Manager  --REST-->  zabbix-nsx-t-exporter  --/metrics-->  Zabbix 7.0
                              (Go, Prometheus format)               (HTTP agent +
                                                                     prometheus.pattern)
```

Zabbix scrapes the exporter once per interval (default 60s), then fans the
response out to dependent items and LLD rules. All metric extraction is
done by Zabbix's native `prometheus.pattern` / `prometheus.to_json`
preprocessors — no external sidecars or pushers.

## Build

```
go build -o nsx-t-exporter .
```

Or via Docker:

```
docker build -t nsx-t-exporter .
docker compose up -d
```

## Configuration

The exporter is configured by environment variables.

| Variable | Default | Description |
|---|---|---|
| `NSXV3_LOGIN_HOST` | (required) | NSX-T manager VIP or DNS name |
| `NSXV3_LOGIN_USER` | (required) | Local user with `Auditor` role |
| `NSXV3_LOGIN_PASSWORD` | (required) | Password |
| `NSXV3_LOGIN_PORT` | 443 | HTTPS port |
| `NSXV3_SUPPRESS_SSL_WARNINGS` | false | Skip TLS verification (do not use in prod) |
| `NSXV3_REQUESTS_PER_SECOND` | 10 | Rate-limit toward NSX-T |
| `NSXV3_REQUEST_TIMEOUT_SECONDS` | 0 (no limit) | Per-request timeout |
| `SCRAP_PORT` | 9999 | HTTP port serving `/metrics` |
| `SCRAP_SCHEDULE_SECONDS` | 0 | Async scrape interval (0 = continuous) |
| `NSXV3_INCLUDE_EDGE_STATS` | true | Collect edge node CPU/mem/disk |
| `NSXV3_INCLUDE_TUNNELS` | true | Collect tunnel status |
| `NSXV3_INCLUDE_BGP` | true | Collect BGP neighbor status |
| `NSXV3_INCLUDE_IP_POOLS` | true | Collect IP pool usage |
| `NSXV3_INCLUDE_CERTIFICATES` | true | Collect cert expiry |
| `NSXV3_INCLUDE_BACKUP` | true | Collect backup status |
| `NSXV3_INCLUDE_LB` | true | Collect LB service/VS/pool/member |

In large fabrics, the LB and BGP collectors are the heaviest. Disable them
selectively if NSX-T API load is a concern.

## Run

```
export NSXV3_LOGIN_HOST=nsxt-mgr.example.com
export NSXV3_LOGIN_USER=zabbix-readonly
export NSXV3_LOGIN_PASSWORD=...
./nsx-t-exporter
# scrape http://localhost:9999/metrics
```

NSX-T side: create a local user with the `Auditor` role (read-only across
all resources). The exporter does no writes.

## Zabbix 7.0 setup

1. Import [zabbix/template-nsx-t.yaml](zabbix/template-nsx-t.yaml).
2. Create a host, link the template.
3. Set the host macro `{$NSXT.EXPORTER.URL}` to your exporter (e.g.
   `http://nsxt-exporter:9999`).
4. Tune thresholds via the other `{$NSXT.*}` macros (see the template's
   macros section).

See [docs/ZABBIX-TRIGGERS.md](docs/ZABBIX-TRIGGERS.md) for the trigger
reference mapped to the nine scenarios above.

## Repository layout

```
config/              env-driven configuration
exporter/            HTTP client + collectors (one file per feature)
  collector_edge.go     Edge node CPU/mem/disk
  collector_tunnels.go  Geneve/TEP tunnel status
  collector_bgp.go      BGP neighbor walk
  collector_ippools.go  Policy + MP IP pool usage
  collector_certs.go    Certificate expiry
  collector_backup.go   Cluster/node/inventory backup history
  collector_lb.go       Load balancer service / VS / pool / member
zabbix/
  template-nsx-t.yaml   Zabbix 7.0 import
docs/
  METRICS.md            Full metric catalogue
  ZABBIX-TRIGGERS.md    Triggers mapped to operational scenarios
```

## Acknowledgements

- Upstream exporter and collection waveform: SAP Converged Cloud team's
  [sapcc/nsx-t-exporter](https://github.com/sapcc/nsx-t-exporter).
- VMware NSX-T 4.2 API reference for endpoint paths and response schemas.
