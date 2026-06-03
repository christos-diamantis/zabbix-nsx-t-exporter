// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"net/url"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// LBStatisticsCounter mirrors NSX's LBStatisticsCounter schema. The same
// shape appears at every level of the LB hierarchy (service / VS / pool /
// pool member) so a single struct is reused everywhere.
type LBStatisticsCounter struct {
	BytesIn                      float64 `json:"bytes_in"`
	BytesOut                     float64 `json:"bytes_out"`
	CurrentSessions              float64 `json:"current_sessions"`
	HTTPRequests                 float64 `json:"http_requests"`
	MaxSessions                  float64 `json:"max_sessions"`
	PacketsIn                    float64 `json:"packets_in"`
	PacketsOut                   float64 `json:"packets_out"`
	SourceIPPersistenceEntrySize float64 `json:"source_ip_persistence_entry_size"`
	TotalSessions                float64 `json:"total_sessions"`
}

// Nsxv3LBServiceData captures status + stats for one LB service plus all of
// its hosted virtual servers and pools (with pool members).
type Nsxv3LBServiceData struct {
	ID             string
	DisplayName    string
	Status         float64
	Stats          LBStatisticsCounter
	VirtualServers []Nsxv3LBVirtualServerData
	Pools          []Nsxv3LBPoolData
}

type Nsxv3LBVirtualServerData struct {
	ID          string
	DisplayName string
	LBServiceID string
	Status      float64
	Stats       LBStatisticsCounter
}

type Nsxv3LBPoolData struct {
	ID          string
	DisplayName string
	Status      float64
	Stats       LBStatisticsCounter
	Members     []Nsxv3LBPoolMemberData
}

type Nsxv3LBPoolMemberData struct {
	IP    string
	Port  string
	Stats LBStatisticsCounter
}

var lbServiceStates = map[string]float64{
	"UP":         1,
	"NO_STANDBY": 0.5,
	"DOWN":       0,
	"DEGRADED":   -1,
	"DISABLED":   -2,
	"UNKNOWN":    -3,
}
var lbVSStates = map[string]float64{
	"UP":           1,
	"PARTIALLY_UP": 0.5,
	"DOWN":         0,
	"PRIMARY_DOWN": -1,
	"DETACHED":     -2,
	"DISABLED":     -3,
	"UNKNOWN":      -4,
}
var lbPoolStates = map[string]float64{
	"UP":           1,
	"PARTIALLY_UP": 0.5,
	"DOWN":         0,
	"PRIMARY_DOWN": -1,
	"DETACHED":     -2,
	"UNKNOWN":      -3,
}
// Response shapes for /lb-services/<id>/detailed-status and /statistics.
// Both wrap their payload in results[]; the relevant entry is results[0].

type lbServiceDetailedStatus struct {
	Results []struct {
		ServiceStatus  string `json:"service_status"`
		VirtualServers []struct {
			VirtualServerPath string `json:"virtual_server_path"`
			Status            string `json:"status"`
		} `json:"virtual_servers"`
		Pools []struct {
			PoolPath string `json:"pool_path"`
			Status   string `json:"status"`
			// Note: members[] is documented in the schema but is empty
			// on NSX-T 4.2 for NCP-managed pools. We populate the member
			// list from the /statistics response instead.
		} `json:"pools"`
	} `json:"results"`
}

type lbServiceStatistics struct {
	Results []struct {
		Statistics     LBStatisticsCounter `json:"statistics"`
		VirtualServers []struct {
			VirtualServerPath string              `json:"virtual_server_path"`
			Statistics        LBStatisticsCounter `json:"statistics"`
		} `json:"virtual_servers"`
		Pools []struct {
			PoolPath   string              `json:"pool_path"`
			Statistics LBStatisticsCounter `json:"statistics"`
			Members    []struct {
				IPAddress  string              `json:"ip_address"`
				Port       string              `json:"port"`
				Statistics LBStatisticsCounter `json:"statistics"`
			} `json:"members"`
		} `json:"pools"`
	} `json:"results"`
}

// Top-level VS list — used only to map virtual_server_path → display name.
type lbVSListItem struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Path          string `json:"path"`
	LBServicePath string `json:"lb_service_path"`
}

type lbVSList struct {
	Results []lbVSListItem `json:"results"`
	Cursor  string         `json:"cursor"`
}

func collectLB(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	services, err := listPolicyResources(ctx, client, "/policy/api/v1/infra/lb-services")
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}

	// Build path → display_name maps. The service detailed-status response
	// references VS and pools only by path, so we need both lookup tables.
	vsByPath, err := buildLBVSPathMap(ctx, client)
	if err != nil {
		log.Warnf("LB virtual server list failed: %v", err)
	}
	poolByPath, err := buildLBPoolPathMap(ctx, client)
	if err != nil {
		log.Warnf("LB pool list failed: %v", err)
	}

	// Some pools are referenced by virtual servers in multiple LB services
	// (rare, but possible). Dedupe pool emissions across services so each
	// pool/member series appears exactly once.
	var poolSeen sync.Map

	out := make([]Nsxv3LBServiceData, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Add(1)
		go func(idx int, id, name string) {
			defer wg.Done()
			out[idx] = fetchLBService(ctx, client, id, name, vsByPath, poolByPath, &poolSeen)
		}(i, svc.ID, svc.DisplayName)
	}
	wg.Wait()

	data.LBServices = out
	return nil
}

// buildLBVSPathMap enumerates all virtual servers and returns a
// path → (id, display_name, lb_service_id) lookup table.
func buildLBVSPathMap(ctx context.Context, client *Nsxv3Client) (map[string]lbVSListItem, error) {
	all, err := listLBVirtualServers(ctx, client)
	if err != nil {
		return nil, err
	}
	out := make(map[string]lbVSListItem, len(all))
	for _, vs := range all {
		if vs.Path != "" {
			out[vs.Path] = vs
		}
	}
	return out, nil
}

type lbPoolListItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Path        string `json:"path"`
}

type lbPoolList struct {
	Results []lbPoolListItem `json:"results"`
	Cursor  string           `json:"cursor"`
}

func buildLBPoolPathMap(ctx context.Context, client *Nsxv3Client) (map[string]lbPoolListItem, error) {
	var all []lbPoolListItem
	cursor := ""
	for {
		p := "/policy/api/v1/infra/lb-pools?page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp lbPoolList
		if err := client.Get(ctx, p, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Results...)
		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}
	out := make(map[string]lbPoolListItem, len(all))
	for _, p := range all {
		if p.Path != "" {
			out[p.Path] = p
		}
	}
	return out, nil
}

func listLBVirtualServers(ctx context.Context, client *Nsxv3Client) ([]lbVSListItem, error) {
	var all []lbVSListItem
	cursor := ""
	for {
		p := "/policy/api/v1/infra/lb-virtual-servers?page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp lbVSList
		if err := client.Get(ctx, p, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Results...)
		if resp.Cursor == "" {
			return all, nil
		}
		cursor = resp.Cursor
	}
}

// fetchLBService performs the two service-scoped calls that hold the entire
// status + stats tree for a single LB service: /detailed-status and
// /statistics. The pools and virtual servers seen here are matched to the
// pre-built path maps for display names.
func fetchLBService(
	ctx context.Context,
	client *Nsxv3Client,
	svcID, svcName string,
	vsByPath map[string]lbVSListItem,
	poolByPath map[string]lbPoolListItem,
	poolSeen *sync.Map,
) Nsxv3LBServiceData {
	out := Nsxv3LBServiceData{
		ID:          svcID,
		DisplayName: svcName,
		Status:      lbServiceStates["UNKNOWN"],
	}
	// URL-encode the path segment because policy LB service IDs frequently
	// contain colons (e.g. "clusterip_domain-c36:640d0752-...").
	encSvc := url.PathEscape(svcID)

	// 1. Detailed status — service + per-VS + per-pool status.
	// Members are NOT included in this response on NCP-managed LBs; we
	// collect them from /statistics instead (no status field there).
	var ds lbServiceDetailedStatus
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-services/"+encSvc+"/detailed-status", &ds); err != nil {
		log.Warnf("LB service %s detailed-status failed: %v", svcID, err)
	} else if len(ds.Results) > 0 {
		r := ds.Results[0]
		if v, ok := lbServiceStates[r.ServiceStatus]; ok {
			out.Status = v
		}
		for _, v := range r.VirtualServers {
			vsMeta := vsByPath[v.VirtualServerPath]
			out.VirtualServers = append(out.VirtualServers, Nsxv3LBVirtualServerData{
				ID:          vsMeta.ID,
				DisplayName: vsMeta.DisplayName,
				LBServiceID: svcID,
				Status:      lookupLBState(lbVSStates, v.Status),
			})
		}
		for _, p := range r.Pools {
			if _, already := poolSeen.LoadOrStore(p.PoolPath, true); already {
				continue
			}
			poolMeta := poolByPath[p.PoolPath]
			out.Pools = append(out.Pools, Nsxv3LBPoolData{
				ID:          poolMeta.ID,
				DisplayName: poolMeta.DisplayName,
				Status:      lookupLBState(lbPoolStates, p.Status),
			})
		}
	}

	// 2. Statistics — service aggregate + per-VS + per-pool + per-member.
	// This is the only place pool members appear (with counters but no
	// status; see the field-test notes in docs/METRICS.md).
	var st lbServiceStatistics
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-services/"+encSvc+"/statistics", &st); err != nil {
		log.Warnf("LB service %s statistics failed: %v", svcID, err)
		return out
	}
	if len(st.Results) == 0 {
		return out
	}
	r := st.Results[0]
	out.Stats = r.Statistics

	// Match VS statistics by path → ID.
	vsStatsByPath := make(map[string]LBStatisticsCounter, len(r.VirtualServers))
	for _, v := range r.VirtualServers {
		vsStatsByPath[v.VirtualServerPath] = v.Statistics
	}
	for i := range out.VirtualServers {
		for path, meta := range vsByPath {
			if meta.ID == out.VirtualServers[i].ID {
				out.VirtualServers[i].Stats = vsStatsByPath[path]
				break
			}
		}
	}

	// Match pool statistics by path, and populate members from the same
	// response (the detailed-status response carries no members for
	// NCP-managed pools).
	for _, p := range r.Pools {
		poolMeta := poolByPath[p.PoolPath]
		idx := -1
		for i, pool := range out.Pools {
			if pool.ID == poolMeta.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue // pool was claimed by another service; skip
		}
		out.Pools[idx].Stats = p.Statistics
		for _, m := range p.Members {
			out.Pools[idx].Members = append(out.Pools[idx].Members, Nsxv3LBPoolMemberData{
				IP:    m.IPAddress,
				Port:  m.Port,
				Stats: m.Statistics,
			})
		}
	}

	return out
}

// lookupLBState returns the numeric value for a textual LB state, falling
// back to that subsystem's UNKNOWN value when the state string is empty or
// unrecognised.
func lookupLBState(table map[string]float64, key string) float64 {
	if v, ok := table[key]; ok {
		return v
	}
	return table["UNKNOWN"]
}

// lbSubsystemKey maps the metric subsystem name to the CamelCase prefix used
// when storing descriptors in the APIMetrics map.
var lbSubsystemKey = map[string]string{
	"lb_service":        "LbService",
	"lb_virtual_server": "LbVirtualServer",
	"lb_pool":           "LbPool",
	"lb_pool_member":    "LbPoolMember",
}

func registerLBMetrics(m map[string]*prometheus.Desc) {
	svcLabels := []string{NSXV3_MANAGER_HOSTNAME, "lb_service_id", "lb_service_name"}
	vsLabels := []string{NSXV3_MANAGER_HOSTNAME, "lb_service_id", "virtual_server_id", "virtual_server_name"}
	poolLabels := []string{NSXV3_MANAGER_HOSTNAME, "pool_id", "pool_name"}
	memLabels := []string{NSXV3_MANAGER_HOSTNAME, "pool_id", "pool_name", "member_ip", "member_port"}

	m["LBServiceStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_service", "status"),
		"NSX-T LB service status - UP=1, NO_STANDBY=0.5, DOWN=0, DEGRADED=-1, DISABLED=-2, UNKNOWN=-3", svcLabels, nil)
	m["LBVirtualServerStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_virtual_server", "status"),
		"NSX-T LB virtual server status - UP=1, PARTIALLY_UP=0.5, DOWN=0, PRIMARY_DOWN=-1, DETACHED=-2, DISABLED=-3, UNKNOWN=-4", vsLabels, nil)
	m["LBPoolStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_pool", "status"),
		"NSX-T LB pool status - UP=1, PARTIALLY_UP=0.5, DOWN=0, PRIMARY_DOWN=-1, DETACHED=-2, UNKNOWN=-3", poolLabels, nil)
	// Note: nsxv3_lb_pool_member_status is intentionally absent. NSX-T 4.2
	// does not expose per-member operational state for NCP-managed pools
	// through any policy endpoint we have been able to reach. Detect down
	// members via the parent pool's status going PARTIALLY_UP, or via a
	// nodata trigger on the member's _current_sessions counter.

	registerLBCounterSet(m, "lb_service", svcLabels)
	registerLBCounterSet(m, "lb_virtual_server", vsLabels)
	registerLBCounterSet(m, "lb_pool", poolLabels)
	registerLBCounterSet(m, "lb_pool_member", memLabels)
}

func registerLBCounterSet(m map[string]*prometheus.Desc, subsystem string, labels []string) {
	prefix := lbSubsystemKey[subsystem]

	m[prefix+"SessionsTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "sessions_total"),
		"NSX-T LB total sessions counter (cumulative)", labels, nil)
	m[prefix+"CurrentSessions"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "current_sessions"),
		"NSX-T LB current concurrent sessions", labels, nil)
	m[prefix+"MaxSessions"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "max_sessions"),
		"NSX-T LB historical peak concurrent sessions", labels, nil)
	m[prefix+"BytesInTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "bytes_in_total"),
		"NSX-T LB bytes received counter (cumulative)", labels, nil)
	m[prefix+"BytesOutTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "bytes_out_total"),
		"NSX-T LB bytes sent counter (cumulative)", labels, nil)
	m[prefix+"PacketsInTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "packets_in_total"),
		"NSX-T LB packets received counter (cumulative)", labels, nil)
	m[prefix+"PacketsOutTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "packets_out_total"),
		"NSX-T LB packets sent counter (cumulative)", labels, nil)
	m[prefix+"HTTPRequestsTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "http_requests_total"),
		"NSX-T LB HTTP requests counter (cumulative; L7 virtual servers only - 0 for L4)", labels, nil)
	m[prefix+"SourceIPPersistenceEntries"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", subsystem, "source_ip_persistence_entries"),
		"NSX-T LB current source-IP persistence table entry count", labels, nil)
}

func (e *Exporter) emitLBMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, svc := range data.LBServices {
		svcLabels := []string{host, svc.ID, svc.DisplayName}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["LBServiceStatus"], prometheus.GaugeValue, svc.Status, svcLabels...)
		emitLBStats(e.APIMetrics, "LbService", svc.Stats, svcLabels, ch)

		for _, vs := range svc.VirtualServers {
			if vs.ID == "" {
				continue // unmatched VS in response (shouldn't happen in practice)
			}
			vsLabels := []string{host, vs.LBServiceID, vs.ID, vs.DisplayName}
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["LBVirtualServerStatus"], prometheus.GaugeValue, vs.Status, vsLabels...)
			emitLBStats(e.APIMetrics, "LbVirtualServer", vs.Stats, vsLabels, ch)
		}

		for _, pool := range svc.Pools {
			if pool.ID == "" {
				continue
			}
			poolLabels := []string{host, pool.ID, pool.DisplayName}
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["LBPoolStatus"], prometheus.GaugeValue, pool.Status, poolLabels...)
			emitLBStats(e.APIMetrics, "LbPool", pool.Stats, poolLabels, ch)
			for _, mem := range pool.Members {
				memLabels := []string{host, pool.ID, pool.DisplayName, mem.IP, mem.Port}
				// Member status is unavailable on this NSX-T release; we
				// emit counters only. See registerLBMetrics for context.
				emitLBStats(e.APIMetrics, "LbPoolMember", mem.Stats, memLabels, ch)
			}
		}
	}
}

func emitLBStats(metrics map[string]*prometheus.Desc, prefix string, s LBStatisticsCounter, labels []string, ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"SessionsTotal"], prometheus.CounterValue, s.TotalSessions, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"CurrentSessions"], prometheus.GaugeValue, s.CurrentSessions, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"MaxSessions"], prometheus.GaugeValue, s.MaxSessions, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"BytesInTotal"], prometheus.CounterValue, s.BytesIn, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"BytesOutTotal"], prometheus.CounterValue, s.BytesOut, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"PacketsInTotal"], prometheus.CounterValue, s.PacketsIn, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"PacketsOutTotal"], prometheus.CounterValue, s.PacketsOut, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"HTTPRequestsTotal"], prometheus.CounterValue, s.HTTPRequests, labels...)
	ch <- prometheus.MustNewConstMetric(metrics[prefix+"SourceIPPersistenceEntries"], prometheus.GaugeValue, s.SourceIPPersistenceEntrySize, labels...)
}

