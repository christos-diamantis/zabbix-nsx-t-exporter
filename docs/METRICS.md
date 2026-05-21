# Metric catalogue

All metrics use the `nsxv3_` prefix to stay compatible with the upstream
sapcc exporter. Every metric carries an `nsxv3_manager_hostname` label so a
Prometheus or Zabbix host that scrapes multiple NSX-T managers can
distinguish them.

## Cluster (inherited from upstream)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `nsxv3_cluster_management_status` | gauge | host | STABLE=1, INITIALIZING=0, UNSTABLE=-1, DEGRADED=-2, UNKNOWN=-3 |
| `nsxv3_cluster_control_status` | gauge | host | Control-plane variant of the above |
| `nsxv3_cluster_management_online_nodes` | gauge | host | Online manager count |
| `nsxv3_cluster_management_offline_nodes` | gauge | host | Offline manager count |
| `nsxv3_cluster_management_last_successful_data_fetch` | gauge | host | Exporter heartbeat (unixtime) |

## Manager node

| Metric | Type | Labels | Description |
|---|---|---|---|
| `nsxv3_management_node_connectivity` | gauge | host, node_ip | CONNECTED=1, DISCONNECTED=0 |
| `nsxv3_management_node_cpu_cores` | gauge | host, node_ip | CPU core count |
| `nsxv3_management_node_load_average` | gauge | host, node_ip, minutes | 1/5/15-min load average |
| `nsxv3_management_node_cpu_usage_ratio` | gauge | host, node_ip | **New.** `load_avg[0] / cpu_cores`. >0.8 = pressure |
| `nsxv3_management_node_memory_use` / `_total` / `_cached` | gauge | host, node_ip | Memory in bytes |
| `nsxv3_management_node_swap_use` / `_total` | gauge | host, node_ip | Swap in bytes |
| `nsxv3_management_node_storage_use` / `_total` | gauge | host, node_ip, filesystem | Storage in bytes |

## Transport node (inherited)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `nsxv3_transport_node_state` | gauge | host, node_id | Realisation state |
| `nsxv3_transport_node_deployment_state` | gauge | host, node_id | Deployment state |
| `nsxv3_transport_nodes_{up,down,degraded,unknown}` | gauge | host | Aggregate counts |

## Edge node (new in this fork)

Labels: `nsxv3_manager_hostname`, `nsxv3_node_id`, `display_name`. Storage
metrics add `filesystem`.

| Metric | Type | Description |
|---|---|---|
| `nsxv3_edge_node_cpu_cores` | gauge | Provisioned cores |
| `nsxv3_edge_node_load_average_1m` | gauge | 1-min load average |
| `nsxv3_edge_node_cpu_usage_ratio` | gauge | `load_avg / cpu_cores` |
| `nsxv3_edge_node_memory_use` / `_total` | gauge | Memory in bytes |
| `nsxv3_edge_node_memory_usage_ratio` | gauge | `used / total` |
| `nsxv3_edge_node_swap_use` / `_total` | gauge | Swap in bytes |
| `nsxv3_edge_node_uptime_seconds` | gauge | Uptime since last boot |
| `nsxv3_edge_node_storage_use` / `_total` | gauge | Per-filesystem usage |
| `nsxv3_edge_node_storage_usage_ratio` | gauge | Per-filesystem `used / total` |

## Tunnels (new)

Labels: `local_node_id`, `local_node_name`, `local_ip`, `remote_node_id`,
`remote_ip`, `encap`. `local_ip` is part of the key because multi-TEP edges
emit one tunnel entry per source TEP.

| Metric | Type | Description |
|---|---|---|
| `nsxv3_tunnel_status` | gauge | UP=1, DEGRADED=0.5, DOWN=0, UNKNOWN=-1 |
| `nsxv3_tunnel_bfd_diagnostic_code` | gauge | RFC 5880 BFD diagnostic (0 = none) |

## BGP neighbors (new, opt-in via `NSXV3_INCLUDE_BGP`)

Labels: `tier0_id`, `tier0_name`, `locale_service_id`, `neighbor_address`,
`remote_as`, `edge_node_id`. Each configured BGP neighbor has one session
per edge node — the `edge_node_id` label is required for fault localisation
and prevents collisions when multiple edges peer with the same neighbor.

| Metric | Type | Description |
|---|---|---|
| `nsxv3_bgp_neighbor_status` | gauge | ESTABLISHED=1, OPEN_CONFIRM=0.8, OPEN_SENT=0.6, ACTIVE=0.4, CONNECT=0.2, IDLE=0, INVALID/UNKNOWN<0 |
| `nsxv3_bgp_neighbor_in_prefix_count` | gauge | Received prefixes |
| `nsxv3_bgp_neighbor_out_prefix_count` | gauge | Advertised prefixes |
| `nsxv3_bgp_neighbor_messages_received_total` | counter | Cumulative BGP messages in |
| `nsxv3_bgp_neighbor_messages_sent_total` | counter | Cumulative BGP messages out |
| `nsxv3_bgp_neighbor_established_seconds` | gauge | Seconds since current session entered ESTABLISHED (0 otherwise) |
| `nsxv3_bgp_neighbor_connection_drop_count` | counter | Cumulative count of session drops. Rate over a window detects flapping |
| `nsxv3_bgp_neighbor_established_connection_count` | counter | Cumulative count of times the session reached ESTABLISHED |
| `nsxv3_bgp_neighbor_hold_time_seconds` | gauge | Negotiated BGP hold timer |
| `nsxv3_bgp_neighbor_keep_alive_seconds` | gauge | Negotiated keepalive interval |

## IP pools (new)

Labels: `pool_id`, `pool_name`, `source` (MP or POLICY).

| Metric | Type | Description |
|---|---|---|
| `nsxv3_ip_pool_total` | gauge | Total addresses |
| `nsxv3_ip_pool_allocated` | gauge | Allocated addresses |
| `nsxv3_ip_pool_free` | gauge | Free addresses |
| `nsxv3_ip_pool_usage_ratio` | gauge | `allocated / total`. >0.85 = warn, >0.95 = critical |

`source=MP` covers TEP/host-overlay pools. `source=POLICY` covers
segment/VM allocation pools.

## Certificates (new)

Labels: `cert_id`, `cert_name`, `resource_type`, `used_by` (comma-joined).

| Metric | Type | Description |
|---|---|---|
| `nsxv3_certificate_seconds_to_expiry` | gauge | Seconds remaining until `not_after`. Negative if already expired |

## Backup (new)

Labels: `type` ∈ {cluster, node, inventory}.

| Metric | Type | Description |
|---|---|---|
| `nsxv3_backup_last_success_timestamp` | gauge | UNIX timestamp of most recent successful backup; 0 if never succeeded |
| `nsxv3_backup_last_attempt_success` | gauge | 1=success, 0=failure, -1=no attempts recorded |
| `nsxv3_backup_consecutive_failures` | gauge | Failures since last success |

## Load balancer (new, opt-in via `NSXV3_INCLUDE_LB`)

Counter and gauge metrics are emitted at four hierarchy levels with
matching name suffixes:

- `nsxv3_lb_service_*` — labels: `lb_service_id`, `lb_service_name`
- `nsxv3_lb_virtual_server_*` — labels: `lb_service_id`, `virtual_server_id`, `virtual_server_name`
- `nsxv3_lb_pool_*` — labels: `pool_id`, `pool_name`
- `nsxv3_lb_pool_member_*` — labels: `pool_id`, `pool_name`, `member_ip`, `member_port`

Per level:

| Suffix | Type | Description |
|---|---|---|
| `_status` | gauge | UP=1, see metric help for full mapping per level |
| `_sessions_total` | counter | Cumulative session count |
| `_current_sessions` | gauge | Concurrent sessions now |
| `_max_sessions` | gauge | Historical peak |
| `_bytes_in_total` / `_bytes_out_total` | counter | Cumulative bytes |
| `_packets_in_total` / `_packets_out_total` | counter | Cumulative packets |
| `_http_requests_total` | counter | Cumulative HTTP requests (L7 only; 0 for L4) |
| `_source_ip_persistence_entries` | gauge | Source-IP persistence table size |

In Zabbix, use the `Change per second` preprocessor on top of
`prometheus.pattern` for the `_total` counters to derive bandwidth and
session rates.
