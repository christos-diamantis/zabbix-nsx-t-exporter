// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// LBStatisticsCounter mirrors NSX's LBStatisticsCounter schema. The same
// shape appears under LB service, virtual server, pool, and pool member
// statistics responses, so a single struct is reused everywhere.
type LBStatisticsCounter struct {
	BytesIn                       float64 `json:"bytes_in"`
	BytesOut                      float64 `json:"bytes_out"`
	CurrentSessions               float64 `json:"current_sessions"`
	HTTPRequests                  float64 `json:"http_requests"`
	MaxSessions                   float64 `json:"max_sessions"`
	PacketsIn                     float64 `json:"packets_in"`
	PacketsOut                    float64 `json:"packets_out"`
	SourceIPPersistenceEntrySize  float64 `json:"source_ip_persistence_entry_size"`
	TotalSessions                 float64 `json:"total_sessions"`
}

// Nsxv3LBServiceData captures status + stats for one LB service. Each
// service hosts N VSes and references M pools.
type Nsxv3LBServiceData struct {
	ID          string
	DisplayName string
	Status      float64
	Stats       LBStatisticsCounter
	VirtualServers []Nsxv3LBVirtualServerData
	Pools          []Nsxv3LBPoolData
}

type Nsxv3LBVirtualServerData struct {
	ID            string
	DisplayName   string
	LBServiceID   string
	Status        float64
	Stats         LBStatisticsCounter
}

type Nsxv3LBPoolData struct {
	ID          string
	DisplayName string
	Status      float64
	Stats       LBStatisticsCounter
	Members     []Nsxv3LBPoolMemberData
}

type Nsxv3LBPoolMemberData struct {
	IP     string
	Port   string
	Status float64
	Stats  LBStatisticsCounter
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
	"UP":            1,
	"PARTIALLY_UP":  0.5,
	"DOWN":          0,
	"PRIMARY_DOWN":  -1,
	"DETACHED":      -2,
	"DISABLED":      -3,
	"UNKNOWN":       -4,
}
var lbPoolStates = map[string]float64{
	"UP":            1,
	"PARTIALLY_UP":  0.5,
	"DOWN":          0,
	"PRIMARY_DOWN":  -1,
	"DETACHED":      -2,
	"UNKNOWN":       -3,
}
var lbMemberStates = map[string]float64{
	"UP":                1,
	"DOWN":              0,
	"DISABLED":          -1,
	"GRACEFUL_DISABLED": -2,
	"UNUSED":            -3,
	"UNKNOWN":           -4,
}

// Response containers per NSX-T policy LB API schema.
type lbServiceStatusContainer struct {
	Results []struct {
		ServiceStatus string `json:"service_status"`
	} `json:"results"`
}

type lbServiceStatisticsContainer struct {
	Results []struct {
		Statistics LBStatisticsCounter `json:"statistics"`
	} `json:"results"`
}

type lbVSStatusContainer struct {
	Results []struct {
		Status string `json:"status"`
	} `json:"results"`
}

type lbVSStatisticsContainer struct {
	Results []struct {
		Statistics LBStatisticsCounter `json:"statistics"`
	} `json:"results"`
}

type lbVSListItem struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	LBServicePath string `json:"lb_service_path"`
}

type lbVSList struct {
	Results []lbVSListItem `json:"results"`
	Cursor  string         `json:"cursor"`
}

type lbPoolStatusMember struct {
	IPAddress string `json:"ip_address"`
	Port      string `json:"port"`
	Status    string `json:"status"`
}

type lbPoolStatusContainer struct {
	Results []struct {
		Status  string               `json:"status"`
		Members []lbPoolStatusMember `json:"members"`
	} `json:"results"`
}

type lbPoolStatisticsMember struct {
	IPAddress  string              `json:"ip_address"`
	Port       string              `json:"port"`
	Statistics LBStatisticsCounter `json:"statistics"`
}

type lbPoolStatisticsContainer struct {
	Results []struct {
		Statistics LBStatisticsCounter      `json:"statistics"`
		Members    []lbPoolStatisticsMember `json:"members"`
	} `json:"results"`
}

func collectLB(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	services, err := listPolicyResources(ctx, client, "/policy/api/v1/infra/lb-services")
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return nil
	}

	// Enumerate VS + pools once at the top so we can fan out concurrently.
	allVS, err := listLBVirtualServers(ctx, client)
	if err != nil {
		log.Warnf("LB virtual server list failed: %v", err)
	}
	allPools, err := listPolicyResources(ctx, client, "/policy/api/v1/infra/lb-pools")
	if err != nil {
		log.Warnf("LB pool list failed: %v", err)
	}

	// Index VS by their parent LB service for nesting in the data struct.
	vsByService := map[string][]Nsxv3LBVirtualServerData{}

	// Fetch VS status/stats concurrently.
	{
		results := make([]Nsxv3LBVirtualServerData, len(allVS))
		var wg sync.WaitGroup
		for i, vs := range allVS {
			wg.Add(1)
			go func(idx int, vs lbVSListItem) {
				defer wg.Done()
				results[idx] = fetchLBVirtualServer(ctx, client, vs)
			}(i, vs)
		}
		wg.Wait()
		for _, vs := range results {
			vsByService[vs.LBServiceID] = append(vsByService[vs.LBServiceID], vs)
		}
	}

	// Fetch pools concurrently. Pools are not strictly per-service so we
	// collect them flat and attach to every service that references one
	// via a virtual server's pool_path — but that mapping is expensive to
	// resolve. For Zabbix the pool/member metrics are independently useful,
	// so we attach them to data.LBServices[0] under a sentinel ID. The
	// emit code reads from a flat pool list to avoid duplication.
	flatPools := make([]Nsxv3LBPoolData, len(allPools))
	{
		var wg sync.WaitGroup
		for i, p := range allPools {
			wg.Add(1)
			go func(idx int, id, name string) {
				defer wg.Done()
				flatPools[idx] = fetchLBPool(ctx, client, id, name)
			}(i, p.ID, p.DisplayName)
		}
		wg.Wait()
	}

	// Fetch per-LB-service status and stats.
	out := make([]Nsxv3LBServiceData, len(services))
	{
		var wg sync.WaitGroup
		for i, svc := range services {
			wg.Add(1)
			go func(idx int, id, name string) {
				defer wg.Done()
				out[idx] = fetchLBService(ctx, client, id, name)
			}(i, svc.ID, svc.DisplayName)
		}
		wg.Wait()
	}
	for i := range out {
		out[i].VirtualServers = vsByService[out[i].ID]
	}
	// Attach the flat pool list to the first service to keep one source of
	// truth — pool metrics are emitted standalone via their own labels.
	if len(out) > 0 {
		out[0].Pools = flatPools
	}
	data.LBServices = out
	return nil
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

func fetchLBService(ctx context.Context, client *Nsxv3Client, id, name string) Nsxv3LBServiceData {
	out := Nsxv3LBServiceData{ID: id, DisplayName: name, Status: lbServiceStates["UNKNOWN"]}
	var sc lbServiceStatusContainer
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-services/"+id+"/detailed-status", &sc); err == nil && len(sc.Results) > 0 {
		if v, ok := lbServiceStates[sc.Results[0].ServiceStatus]; ok {
			out.Status = v
		}
	} else if err != nil {
		// Fall back to plain /status endpoint name on older 4.x builds.
		_ = client.Get(ctx, "/policy/api/v1/infra/lb-services/"+id+"/status", &sc)
		if len(sc.Results) > 0 {
			if v, ok := lbServiceStates[sc.Results[0].ServiceStatus]; ok {
				out.Status = v
			}
		}
	}
	var st lbServiceStatisticsContainer
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-services/"+id+"/statistics", &st); err == nil && len(st.Results) > 0 {
		out.Stats = st.Results[0].Statistics
	}
	return out
}

func fetchLBVirtualServer(ctx context.Context, client *Nsxv3Client, vs lbVSListItem) Nsxv3LBVirtualServerData {
	out := Nsxv3LBVirtualServerData{
		ID:          vs.ID,
		DisplayName: vs.DisplayName,
		LBServiceID: parseLBServiceIDFromPath(vs.LBServicePath),
		Status:      lbVSStates["UNKNOWN"],
	}
	var sc lbVSStatusContainer
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-virtual-servers/"+vs.ID+"/detailed-status", &sc); err == nil && len(sc.Results) > 0 {
		if v, ok := lbVSStates[sc.Results[0].Status]; ok {
			out.Status = v
		}
	}
	var st lbVSStatisticsContainer
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-virtual-servers/"+vs.ID+"/statistics", &st); err == nil && len(st.Results) > 0 {
		out.Stats = st.Results[0].Statistics
	}
	return out
}

func fetchLBPool(ctx context.Context, client *Nsxv3Client, id, name string) Nsxv3LBPoolData {
	out := Nsxv3LBPoolData{ID: id, DisplayName: name, Status: lbPoolStates["UNKNOWN"]}
	var sc lbPoolStatusContainer
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-pools/"+id+"/detailed-status", &sc); err == nil && len(sc.Results) > 0 {
		r := sc.Results[0]
		if v, ok := lbPoolStates[r.Status]; ok {
			out.Status = v
		}
		for _, m := range r.Members {
			mState, ok := lbMemberStates[m.Status]
			if !ok {
				mState = lbMemberStates["UNKNOWN"]
			}
			out.Members = append(out.Members, Nsxv3LBPoolMemberData{
				IP: m.IPAddress, Port: m.Port, Status: mState,
			})
		}
	}
	var st lbPoolStatisticsContainer
	if err := client.Get(ctx, "/policy/api/v1/infra/lb-pools/"+id+"/statistics", &st); err == nil && len(st.Results) > 0 {
		r := st.Results[0]
		out.Stats = r.Statistics
		// Merge per-member statistics into existing member entries by IP:port.
		for _, mStat := range r.Members {
			matched := false
			for i := range out.Members {
				if out.Members[i].IP == mStat.IPAddress && out.Members[i].Port == mStat.Port {
					out.Members[i].Stats = mStat.Statistics
					matched = true
					break
				}
			}
			if !matched {
				out.Members = append(out.Members, Nsxv3LBPoolMemberData{
					IP: mStat.IPAddress, Port: mStat.Port,
					Status: lbMemberStates["UNKNOWN"], Stats: mStat.Statistics,
				})
			}
		}
	}
	return out
}

// parseLBServiceIDFromPath extracts the service ID from a path like
// "/infra/lb-services/<id>" or "/policy/api/v1/infra/lb-services/<id>".
func parseLBServiceIDFromPath(path string) string {
	const marker = "/lb-services/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return ""
	}
	tail := path[idx+len(marker):]
	if i := strings.Index(tail, "/"); i >= 0 {
		return tail[:i]
	}
	return tail
}

func registerLBMetrics(m map[string]*prometheus.Desc) {
	svcLabels := []string{NSXV3_MANAGER_HOSTNAME, "lb_service_id", "lb_service_name"}
	vsLabels := []string{NSXV3_MANAGER_HOSTNAME, "lb_service_id", "virtual_server_id", "virtual_server_name"}
	poolLabels := []string{NSXV3_MANAGER_HOSTNAME, "pool_id", "pool_name"}
	memLabels := []string{NSXV3_MANAGER_HOSTNAME, "pool_id", "pool_name", "member_ip", "member_port"}

	// Status metrics
	m["LBServiceStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_service", "status"),
		"NSX-T LB service status - UP=1, NO_STANDBY=0.5, DOWN=0, DEGRADED=-1, DISABLED=-2, UNKNOWN=-3", svcLabels, nil)
	m["LBVirtualServerStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_virtual_server", "status"),
		"NSX-T LB virtual server status - UP=1, PARTIALLY_UP=0.5, DOWN=0, PRIMARY_DOWN=-1, DETACHED=-2, DISABLED=-3, UNKNOWN=-4", vsLabels, nil)
	m["LBPoolStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_pool", "status"),
		"NSX-T LB pool status - UP=1, PARTIALLY_UP=0.5, DOWN=0, PRIMARY_DOWN=-1, DETACHED=-2, UNKNOWN=-3", poolLabels, nil)
	m["LBPoolMemberStatus"] = prometheus.NewDesc(prometheus.BuildFQName("nsxv3", "lb_pool_member", "status"),
		"NSX-T LB pool member status - UP=1, DOWN=0, DISABLED=-1, GRACEFUL_DISABLED=-2, UNUSED=-3, UNKNOWN=-4", memLabels, nil)

	// Counter/gauge metrics — same set replicated across the 4 levels.
	registerLBCounterSet(m, "lb_service", svcLabels)
	registerLBCounterSet(m, "lb_virtual_server", vsLabels)
	registerLBCounterSet(m, "lb_pool", poolLabels)
	registerLBCounterSet(m, "lb_pool_member", memLabels)
}

// lbSubsystemKey maps the metric subsystem name to the CamelCase prefix used
// when storing descriptors in the APIMetrics map. Explicit to avoid the
// deprecated strings.Title.
var lbSubsystemKey = map[string]string{
	"lb_service":        "LbService",
	"lb_virtual_server": "LbVirtualServer",
	"lb_pool":           "LbPool",
	"lb_pool_member":    "LbPoolMember",
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
		"NSX-T LB HTTP requests counter (cumulative; L7 virtual servers only — 0 for L4)", labels, nil)
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
			vsLabels := []string{host, vs.LBServiceID, vs.ID, vs.DisplayName}
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["LBVirtualServerStatus"], prometheus.GaugeValue, vs.Status, vsLabels...)
			emitLBStats(e.APIMetrics, "LbVirtualServer", vs.Stats, vsLabels, ch)
		}

		for _, pool := range svc.Pools {
			poolLabels := []string{host, pool.ID, pool.DisplayName}
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["LBPoolStatus"], prometheus.GaugeValue, pool.Status, poolLabels...)
			emitLBStats(e.APIMetrics, "LbPool", pool.Stats, poolLabels, ch)
			for _, mem := range pool.Members {
				memLabels := []string{host, pool.ID, pool.DisplayName, mem.IP, mem.Port}
				ch <- prometheus.MustNewConstMetric(e.APIMetrics["LBPoolMemberStatus"], prometheus.GaugeValue, mem.Status, memLabels...)
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
