// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// Nsxv3BGPNeighborData holds the per-edge state of a BGP neighbor. Each
// neighbor in NSX-T may have multiple BGP sessions, one per edge node, so
// the per-edge breakdown is necessary for fault localisation.
type Nsxv3BGPNeighborData struct {
	Tier0ID         string
	Tier0Name       string
	LocaleServiceID string
	NeighborID      string
	NeighborAddress string
	RemoteAS        string
	EdgeNodeID      string
	Connection      float64
	InPrefixCount   float64
	OutPrefixCount  float64
	MessagesIn      float64
	MessagesOut     float64
	EstablishedTime float64
}

// bgpConnectionStates maps NSX BgpNeighborStatus.connection_state to a
// numeric metric value. ESTABLISHED is the only "healthy" state.
var bgpConnectionStates = map[string]float64{
	"ESTABLISHED":   1,
	"OPEN_CONFIRM":  0.8,
	"OPEN_SENT":     0.6,
	"ACTIVE":        0.4,
	"CONNECT":       0.2,
	"IDLE":          0,
	"INVALID":       -1,
	"UNKNOWN":       -2,
	"NO_NEIGHBOR":   -3,
}

type policyListResp struct {
	Results []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"results"`
	Cursor string `json:"cursor"`
}

type bgpNeighborListItem struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	NeighborAddr  string `json:"neighbor_address"`
	RemoteAS      string `json:"remote_as_num"`
}

type bgpNeighborList struct {
	Results []bgpNeighborListItem `json:"results"`
	Cursor  string                `json:"cursor"`
}

type bgpNeighborStatusEntry struct {
	EdgeNodeID         string  `json:"edge_node_id"`
	ConnectionState    string  `json:"connection_state"`
	TotalInPrefixCount float64 `json:"total_in_prefix_count"`
	TotalOutPrefixCount float64 `json:"total_out_prefix_count"`
	MessagesReceived   float64 `json:"messages_received"`
	MessagesSent       float64 `json:"messages_sent"`
	TimeSinceEstablished float64 `json:"time_since_established"`
	NeighborAddress    string  `json:"neighbor_address"`
	RemoteASNumber     string  `json:"remote_as_number"`
}

type bgpNeighborStatusResp struct {
	Results []bgpNeighborStatusEntry `json:"results"`
}

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
	if containsRune(path, '?') {
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

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// collectBGPNeighbors walks tier-0 → locale-service → neighbor → /status and
// flattens the per-edge BGP sessions into data.BGPNeighbors.
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
				log.Warnf("BGP locale-service list failed for tier-0 %s: %v", t0ID, err)
				return
			}
			for _, l := range ls {
				neighbors, err := listBGPNeighbors(ctx, client, t0ID, l.ID)
				if err != nil {
					log.Warnf("BGP neighbor list failed for %s/%s: %v", t0ID, l.ID, err)
					continue
				}
				for _, n := range neighbors {
					path := "/policy/api/v1/infra/tier-0s/" + t0ID + "/locale-services/" + l.ID + "/bgp/neighbors/" + n.ID + "/status"
					var st bgpNeighborStatusResp
					if err := client.Get(ctx, path, &st); err != nil {
						log.Warnf("BGP neighbor status failed for %s/%s/%s: %v", t0ID, l.ID, n.ID, err)
						continue
					}
					for _, entry := range st.Results {
						state, ok := bgpConnectionStates[entry.ConnectionState]
						if !ok {
							state = bgpConnectionStates["UNKNOWN"]
						}
						addr := entry.NeighborAddress
						if addr == "" {
							addr = n.NeighborAddr
						}
						remoteAS := entry.RemoteASNumber
						if remoteAS == "" {
							remoteAS = n.RemoteAS
						}
						item := Nsxv3BGPNeighborData{
							Tier0ID:         t0ID,
							Tier0Name:       t0Name,
							LocaleServiceID: l.ID,
							NeighborID:      n.ID,
							NeighborAddress: addr,
							RemoteAS:        remoteAS,
							EdgeNodeID:      entry.EdgeNodeID,
							Connection:      state,
							InPrefixCount:   entry.TotalInPrefixCount,
							OutPrefixCount:  entry.TotalOutPrefixCount,
							MessagesIn:      entry.MessagesReceived,
							MessagesOut:     entry.MessagesSent,
							EstablishedTime: entry.TimeSinceEstablished,
						}
						mu.Lock()
						out = append(out, item)
						mu.Unlock()
					}
				}
			}
		}(t0.ID, t0.DisplayName)
	}
	wg.Wait()

	data.BGPNeighbors = out
	return nil
}

func listBGPNeighbors(ctx context.Context, client *Nsxv3Client, tier0ID, localeID string) ([]bgpNeighborListItem, error) {
	var all []bgpNeighborListItem
	cursor := ""
	for {
		p := "/policy/api/v1/infra/tier-0s/" + tier0ID + "/locale-services/" + localeID + "/bgp/neighbors?page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp bgpNeighborList
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

func registerBGPMetrics(m map[string]*prometheus.Desc) {
	labels := []string{NSXV3_MANAGER_HOSTNAME, "tier0_id", "tier0_name", "locale_service_id", "neighbor_id", "neighbor_address", "remote_as", "edge_node_id"}
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
		"NSX-T BGP neighbor BGP messages received",
		labels, nil,
	)
	m["BGPNeighborMessagesOut"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "messages_sent_total"),
		"NSX-T BGP neighbor BGP messages sent",
		labels, nil,
	)
	m["BGPNeighborEstablishedSeconds"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "bgp_neighbor", "established_seconds"),
		"NSX-T BGP neighbor time since current session established, in seconds. 0 when not established",
		labels, nil,
	)
}

func (e *Exporter) emitBGPMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, n := range data.BGPNeighbors {
		lvals := []string{host, n.Tier0ID, n.Tier0Name, n.LocaleServiceID, n.NeighborID, n.NeighborAddress, n.RemoteAS, n.EdgeNodeID}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborStatus"], prometheus.GaugeValue, n.Connection, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborInPrefixCount"], prometheus.GaugeValue, n.InPrefixCount, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborOutPrefixCount"], prometheus.GaugeValue, n.OutPrefixCount, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborMessagesIn"], prometheus.CounterValue, n.MessagesIn, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborMessagesOut"], prometheus.CounterValue, n.MessagesOut, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BGPNeighborEstablishedSeconds"], prometheus.GaugeValue, n.EstablishedTime, lvals...)
	}
}
