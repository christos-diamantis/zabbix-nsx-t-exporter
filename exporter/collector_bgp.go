// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// Nsxv3BGPNeighborData holds the per-edge state of a BGP session. Each
// configured BGP neighbor may have multiple sessions in NSX-T (one per edge
// node in the cluster) so the per-edge breakdown is necessary for fault
// localisation.
type Nsxv3BGPNeighborData struct {
	Tier0ID                    string
	Tier0Name                  string
	LocaleServiceID            string
	NeighborAddress            string
	SourceAddress              string
	RemoteAS                   string
	EdgeNodeID                 string
	Connection                 float64
	InPrefixCount              float64
	OutPrefixCount             float64
	MessagesIn                 float64
	MessagesOut                float64
	EstablishedTime            float64
	ConnectionDropCount        float64
	EstablishedConnectionCount float64
	HoldTime                   float64
	KeepAliveInterval          float64
}

// bgpConnectionStates maps NSX BgpNeighborStatus.connection_state to a
// numeric metric value. ESTABLISHED is the only "healthy" state.
var bgpConnectionStates = map[string]float64{
	"ESTABLISHED":  1,
	"OPEN_CONFIRM": 0.8,
	"OPEN_SENT":    0.6,
	"ACTIVE":       0.4,
	"CONNECT":      0.2,
	"IDLE":         0,
	"INVALID":      -1,
	"UNKNOWN":      -2,
	"NO_NEIGHBOR":  -3,
}

type policyListResp struct {
	Results []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"results"`
	Cursor string `json:"cursor"`
}

// bgpNeighborStatusEntry mirrors one element of the results[] array returned
// by the collection-level /bgp/neighbors/status endpoint. Each element
// represents one BGP session — i.e. one (neighbor, edge node) pairing.
type bgpNeighborStatusEntry struct {
	EdgePath                   string  `json:"edge_path"`
	SourceAddress              string  `json:"source_address"`
	NeighborAddress            string  `json:"neighbor_address"`
	RemoteASNumber             string  `json:"remote_as_number"`
	ConnectionState            string  `json:"connection_state"`
	MessagesReceived           float64 `json:"messages_received"`
	MessagesSent               float64 `json:"messages_sent"`
	TimeSinceEstablished       float64 `json:"time_since_established"`
	TotalInPrefixCount         float64 `json:"total_in_prefix_count"`
	TotalOutPrefixCount        float64 `json:"total_out_prefix_count"`
	ConnectionDropCount        float64 `json:"connection_drop_count"`
	EstablishedConnectionCount float64 `json:"established_connection_count"`
	HoldTime                   float64 `json:"hold_time"`
	KeepAliveInterval          float64 `json:"keep_alive_interval"`
}

type bgpNeighborStatusResp struct {
	Results []bgpNeighborStatusEntry `json:"results"`
}

// listPolicyResources enumerates a paginated policy list endpoint and returns
// the (id, display_name) pairs. Used for tier-0 and locale-service enumeration.
func listPolicyResources(ctx context.Context, client *Nsxv3Client, path string) ([]struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}, error) {
	var all []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	cursor := ""
	sep := "?"
	if strings.ContainsRune(path, '?') {
		sep = "&"
	}
	for {
		p := path + sep + "page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp policyListResp
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

// edgeNodeIDFromPath extracts the edge node UUID from a policy edge_path of
// the form:
//
//	/infra/sites/<site>/enforcement-points/<ep>/edge-clusters/<cluster>/edge-nodes/<edge>
//
// Returns the trailing path segment after /edge-nodes/. Returns the empty
// string if the marker is absent.
func edgeNodeIDFromPath(p string) string {
	const marker = "/edge-nodes/"
	idx := strings.Index(p, marker)
	if idx < 0 {
		return ""
	}
	return p[idx+len(marker):]
}

// collectBGPNeighbors walks tier-0 -> locale-service and fetches the
// collection-level /bgp/neighbors/status for each locale-service. This is
// dramatically more efficient than the per-neighbor /status endpoint, which
// also turns out not to exist in NSX-T 4.2 (it returns 404).
//
// Each (neighbor_address, edge_node_id) tuple from the response becomes one
// emitted series.
func collectBGPNeighbors(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	tier0s, err := listPolicyResources(ctx, client, "/policy/api/v1/infra/tier-0s")
	if err != nil {
		return err
	}
	if len(tier0s) == 0 {
		return nil
	}

	var mu sync.Mutex
	var out []Nsxv3BGPNeighborData
	var wg sync.WaitGroup

	for _, t0 := range tier0s {
		wg.Add(1)
		go func(t0ID, t0Name string) {
			defer wg.Done()
			ls, err := listPolicyResources(ctx, client, "/policy/api/v1/infra/tier-0s/"+t0ID+"/locale-services")
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					log.Warnf("BGP locale-service list failed for tier-0 %s: %v", t0ID, err)
				}
				return
			}
			for _, l := range ls {
				// Collection-level status: returns one entry per active
				// BGP session across all configured neighbors and all edge
				// nodes in the cluster. One call per locale-service.
				path := "/policy/api/v1/infra/tier-0s/" + t0ID +
					"/locale-services/" + l.ID +
					"/bgp/neighbors/status?enforcement_point_path=/infra/sites/default/enforcement-points/default"
				var st bgpNeighborStatusResp
				if err := client.Get(ctx, path, &st); err != nil {
					if errors.Is(err, ErrNotFound) {
						// No BGP configured on this locale-service yet.
						log.Debugf("no BGP status for %s/%s (404)", t0ID, l.ID)
						continue
					}
					log.Warnf("BGP collection-status failed for %s/%s: %v", t0ID, l.ID, err)
					continue
				}
				for _, entry := range st.Results {
					state, ok := bgpConnectionStates[entry.ConnectionState]
					if !ok {
						state = bgpConnectionStates["UNKNOWN"]
					}
					item := Nsxv3BGPNeighborData{
						Tier0ID:                    t0ID,
						Tier0Name:                  t0Name,
						LocaleServiceID:            l.ID,
						NeighborAddress:            entry.NeighborAddress,
						SourceAddress:              entry.SourceAddress,
						RemoteAS:                   entry.RemoteASNumber,
						EdgeNodeID:                 edgeNodeIDFromPath(entry.EdgePath),
						Connection:                 state,
						InPrefixCount:              entry.TotalInPrefixCount,
						OutPrefixCount:             entry.TotalOutPrefixCount,
						MessagesIn:                 entry.MessagesReceived,
						MessagesOut:                entry.MessagesSent,
						EstablishedTime:            entry.TimeSinceEstablished,
						ConnectionDropCount:        entry.ConnectionDropCount,
						EstablishedConnectionCount: entry.EstablishedConnectionCount,
						HoldTime:                   entry.HoldTime,
						KeepAliveInterval:          entry.KeepAliveInterval,
					}
					mu.Lock()
					out = append(out, item)
					mu.Unlock()
				}
			}
		}(t0.ID, t0.DisplayName)
	}
	wg.Wait()

	data.BGPNeighbors = out
	return nil
}

func registerBGPMetrics(m map[string]*prometheus.Desc) {
	// Label set is (tier0, locale_service, neighbor_address, remote_as,
	// edge_node_id) — these are sufficient to uniquely identify any BGP
	// session in the fabric. source_address is exposed as data, not a
	// label, because it can change between sessions (TEP rebinding).
	labels := []string{NSXV3_MANAGER_HOSTNAME, "tier0_id", "tier0_name", "locale_service_id", "neighbor_address", "remote_as", "edge_node_id"}
	m["BGPNeighborStatus"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "status"),
		"NSX-T BGP neighbor connection state - ESTABLISHED=1, OPEN_CONFIRM=0.8, OPEN_SENT=0.6, ACTIVE=0.4, CONNECT=0.2, IDLE=0, INVALID=-1, UNKNOWN=-2",
		labels, nil,
	)
	m["BGPNeighborInPrefixCount"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "in_prefix_count"),
		"NSX-T BGP neighbor count of received prefixes",
		labels, nil,
	)
	m["BGPNeighborOutPrefixCount"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "out_prefix_count"),
		"NSX-T BGP neighbor count of advertised prefixes",
		labels, nil,
	)
	m["BGPNeighborMessagesIn"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "messages_received_total"),
		"NSX-T BGP neighbor cumulative count of BGP messages received",
		labels, nil,
	)
	m["BGPNeighborMessagesOut"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "messages_sent_total"),
		"NSX-T BGP neighbor cumulative count of BGP messages sent",
		labels, nil,
	)
	m["BGPNeighborEstablishedSeconds"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "established_seconds"),
		"NSX-T BGP neighbor seconds since current session entered ESTABLISHED state. 0 when not established",
		labels, nil,
	)
	m["BGPNeighborConnectionDropCount"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "connection_drop_count"),
		"NSX-T BGP neighbor cumulative count of session drops. Increases imply session flapping",
		labels, nil,
	)
	m["BGPNeighborEstablishedConnectionCount"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "established_connection_count"),
		"NSX-T BGP neighbor cumulative count of times session reached ESTABLISHED. >1 + recent connection_drop_count rise = flapping",
		labels, nil,
	)
	m["BGPNeighborHoldTime"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "hold_time_seconds"),
		"NSX-T BGP neighbor negotiated hold timer in seconds",
		labels, nil,
	)
	m["BGPNeighborKeepAliveInterval"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "keep_alive_seconds"),
		"NSX-T BGP neighbor negotiated keepalive interval in seconds",
		labels, nil,
	)
}

func (e *Exporter) emitBGPMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, n := range data.BGPNeighbors {
		lvals := []string{host, n.Tier0ID, n.Tier0Name, n.LocaleServiceID, n.NeighborAddress, n.RemoteAS, n.EdgeNodeID}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborStatus"], prometheus.GaugeValue, n.Connection, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborInPrefixCount"], prometheus.GaugeValue, n.InPrefixCount, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborOutPrefixCount"], prometheus.GaugeValue, n.OutPrefixCount, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborMessagesIn"], prometheus.CounterValue, n.MessagesIn, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborMessagesOut"], prometheus.CounterValue, n.MessagesOut, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborEstablishedSeconds"], prometheus.GaugeValue, n.EstablishedTime, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborConnectionDropCount"], prometheus.CounterValue, n.ConnectionDropCount, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborEstablishedConnectionCount"], prometheus.CounterValue, n.EstablishedConnectionCount, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborHoldTime"], prometheus.GaugeValue, n.HoldTime, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborKeepAliveInterval"], prometheus.GaugeValue, n.KeepAliveInterval, lvals...)
	}
}
