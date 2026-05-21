# Zabbix triggers — operational scenarios reference

Every trigger in `zabbix/template-nsx-t.yaml` is listed below, grouped by
the operational scenario it addresses. Severity is the Zabbix default; tune
in your environment via the `{$NSXT.*}` macros.

## #1 — Manager cluster down/degraded

| Trigger | Severity | Expression |
|---|---|---|
| Manager cluster is degraded or down | HIGH | `last(.../nsxt.cluster.management.status) < 1` |
| Manager control plane is degraded | HIGH | `last(.../nsxt.cluster.control.status) < 1` |
| Manager cluster has offline node(s) | HIGH | `last(.../nsxt.cluster.offline_nodes) > 0` |
| Manager $NODE_IP is not CONNECTED | HIGH | LLD: `last(.../nsxt.mgr.connectivity[{#NODE_IP}]) < 1` |

## #2 — Edge node down

| Trigger | Severity | Expression |
|---|---|---|
| Edge $NODE_NAME has no status | HIGH | `nodata(.../nsxt.edge.cpu_cores[{#NODE_ID}], 5m) = 1` |

Detection is by absence: when NSX-T's API stops returning a node-status
response for an edge, no `nsxv3_edge_node_*` series for it appears in the
next scrape. The Zabbix LLD entry persists for `lifetime: 7d`, so the
`nodata` trigger fires.

## #3 — Transport node disconnected

| Trigger | Severity | Expression |
|---|---|---|
| Transport node $NODE_ID is not healthy | HIGH | LLD: `last(.../nsxt.transport_node.state[{#NODE_ID}]) < 1` |

## #4 — High CPU/Memory on Edge/Manager

| Trigger | Severity | Expression |
|---|---|---|
| Manager $NODE_IP CPU pressure high | WARNING | LLD: `last(.../nsxt.mgr.cpu_usage_ratio[{#NODE_IP}]) > {$NSXT.CPU.WARN}` |
| Manager $NODE_IP memory pressure high | WARNING | LLD: `last(.../nsxt.mgr.memory_usage_ratio[{#NODE_IP}]) > {$NSXT.MEM.WARN}` |
| Edge $NODE_NAME CPU pressure high | WARNING | LLD: `last(.../nsxt.edge.cpu_usage_ratio[{#NODE_ID}]) > {$NSXT.CPU.WARN}` |
| Edge $NODE_NAME memory pressure high | WARNING | LLD: `last(.../nsxt.edge.memory_usage_ratio[{#NODE_ID}]) > {$NSXT.MEM.WARN}` |
| Edge $NODE_NAME FS $FS is filling up | WARNING | LLD: `last(.../nsxt.edge.fs.usage_ratio[{#NODE_ID},{#FS}]) > {$NSXT.DISK.WARN}` |

Macro defaults: CPU.WARN=0.8, MEM.WARN=0.9, DISK.WARN=0.85.

CPU thresholding uses load-average divided by core count — a value of 1.0
means one core's worth of work in the runnable queue per core. 0.8 is a
sustained-pressure indicator that handles bursty workloads well.

## #5 — Geneve/TEP tunnel down

| Trigger | Severity | Expression |
|---|---|---|
| Tunnel $LOCAL_NAME → $REMOTE_IP is not UP | HIGH | LLD: `last(.../nsxt.tunnel.status[...]) < 1` |

The label set includes encap (`GENEVE`/`STT`) and local/remote node IDs so
the trigger localises which pair of transport nodes lost connectivity.

## #6 — BGP neighbor down

| Trigger | Severity | Expression |
|---|---|---|
| BGP $TIER0/$NEIGHBOR_IP on edge $EDGE_ID not ESTABLISHED | HIGH | LLD: `last(.../nsxt.bgp.status[...]) < 1` |

A single neighbor has one session per edge node. Triggers fire per (tier-0,
neighbor, edge) so a partial failure where one edge has BGP but the other
doesn't is visible immediately, not masked by aggregation.

## #7 — IP/TEP pool exhaustion

| Trigger | Severity | Expression |
|---|---|---|
| IP pool $POOL_NAME ($SOURCE) is filling up | WARNING | LLD: `last(.../nsxt.ip_pool.usage_ratio[{#POOL_ID}]) > {$NSXT.IP_POOL.WARN}` |

Default `IP_POOL.WARN = 0.85`. The `$SOURCE` label disambiguates
MP pools (TEP/host overlay — capacity changes are operationally painful)
from policy pools (segment/VM IPs — easy to extend).

## #8 — Certificate expiration

| Trigger | Severity | Expression |
|---|---|---|
| Certificate $CERT_NAME expires in <$CRIT.DAYS days | HIGH | LLD: `last(.../nsxt.cert.seconds_to_expiry[{#CERT_ID}]) < ($NSXT.CERT.CRIT.DAYS * 86400)` |
| Certificate $CERT_NAME expires in <$WARN.DAYS days | WARNING | LLD: warn-but-not-crit window |

Defaults: WARN.DAYS=30, CRIT.DAYS=7.

## #9 — Backup failure

| Trigger | Severity | Expression |
|---|---|---|
| Cluster backup has not succeeded for $H hours | HIGH | `(now() - last(.../nsxt.backup.last_success[cluster])) > ($NSXT.BACKUP.STALE.HOURS * 3600)` |
| Cluster backup failed N times in a row | HIGH | `last(.../nsxt.backup.consecutive_failures[cluster]) >= 2` |

Default `BACKUP.STALE.HOURS = 36`. The "consecutive failures" trigger
catches a backup that is still being attempted but failing, even before
the staleness threshold fires.

## Bonus — Load balancer

| Trigger | Severity | Expression |
|---|---|---|
| LB service $LB_NAME is not UP | HIGH | LLD: `last(.../nsxt.lb.service.status[{#LB_ID}]) < 1` |
| Virtual server $VS_NAME is not UP | HIGH | LLD: `last(.../nsxt.lb.vs.status[{#VS_ID}]) < 1` |
| Pool $POOL_NAME is not UP | HIGH | LLD: `last(.../nsxt.lb.pool.status[{#POOL_ID}]) < 1` |
| Pool member $IP:$PORT of $POOL_NAME is not UP | AVERAGE | LLD: `last(.../nsxt.lb.member.status[...]) < 1` |

Pool members are AVERAGE because a single failed member typically doesn't
take a VS down (the pool routes around it); but you still want to know.
Pool/VS/service unavailability is HIGH because it implies customer-visible
impact.

## Exporter availability

A meta-trigger fires if the exporter itself stops collecting:

| Trigger | Severity | Expression |
|---|---|---|
| Exporter is not collecting fresh data | AVERAGE | `(now() - last(.../nsxt.exporter.last_fetch)) > $NSXT.NO_FETCH.SECONDS` |

When this fires, treat all other NSX-T alerts as potentially stale.
