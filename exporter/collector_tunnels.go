// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// Nsxv3TunnelData holds the state of a single Geneve/STT tunnel between two
// transport nodes.
type Nsxv3TunnelData struct {
	LocalNodeID   string
	LocalNodeName string
	RemoteNodeID  string
	RemoteNodeIP  string
	LocalIP       string
	Encap         string
	Status        float64
	BFDDiagCode   float64
}

var tunnelStates = map[string]float64{
	"UP":       1,
	"DEGRADED": 0.5,
	"DOWN":     0,
	"UNKNOWN":  -1,
}

type tunnelEntry struct {
	Encap                 string  `json:"encap"`
	LocalIP               string  `json:"local_ip"`
	RemoteIP              string  `json:"remote_ip"`
	RemoteNodeID          string  `json:"remote_node_id"`
	RemoteNodeDisplayName string  `json:"remote_node_display_name"`
	Status                string  `json:"status"`
	BFDDiagnosticCode     float64 `json:"bfd_diagnostic_code"`
}

type tunnelsResponse struct {
	Tunnels []tunnelEntry `json:"tunnels"`
}

// collectTunnels fetches /api/v1/transport-nodes/<id>/tunnels for every
// transport node (host + edge) and flattens the result.
func collectTunnels(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	nodes, err := listTransportNodes(ctx, client, "")
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	perNode := make([][]Nsxv3TunnelData, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(idx int, tn transportNode) {
			defer wg.Done()
			var resp tunnelsResponse
			path := "/api/v1/transport-nodes/" + tn.ID + "/tunnels"
			if err := client.Get(ctx, path, &resp); err != nil {
				// 404 = transport node has no tunnels (newly added, isolated, etc).
				// Not actionable; debug only.
				if errors.Is(err, ErrNotFound) {
					log.Debugf("transport node %s has no /tunnels (404)", tn.ID)
					return
				}
				log.Warnf("tunnel fetch failed for transport node %s: %v", tn.ID, err)
				return
			}
			out := make([]Nsxv3TunnelData, 0, len(resp.Tunnels))
			for _, t := range resp.Tunnels {
				status, ok := tunnelStates[t.Status]
				if !ok {
					status = tunnelStates["UNKNOWN"]
				}
				out = append(out, Nsxv3TunnelData{
					LocalNodeID:   tn.ID,
					LocalNodeName: tn.DisplayName,
					RemoteNodeID:  t.RemoteNodeID,
					RemoteNodeIP:  t.RemoteIP,
					LocalIP:       t.LocalIP,
					Encap:         t.Encap,
					Status:        status,
					BFDDiagCode:   t.BFDDiagnosticCode,
				})
			}
			perNode[idx] = out
		}(i, n)
	}
	wg.Wait()

	total := 0
	for _, s := range perNode {
		total += len(s)
	}
	// Flatten and deduplicate. NSX may emit the same logical tunnel multiple
	// times across pages (or the same node may report a tunnel twice with
	// trivially different metadata). Prometheus rejects the entire /metrics
	// scrape if a single (name + labels) pair is emitted more than once, so
	// we collapse here on the full label tuple including local_ip.
	type key struct {
		localID, localIP, remoteID, remoteIP, encap string
	}
	seen := make(map[key]struct{})
	data.Tunnels = make([]Nsxv3TunnelData, 0, total)
	for _, s := range perNode {
		for _, t := range s {
			k := key{t.LocalNodeID, t.LocalIP, t.RemoteNodeID, t.RemoteNodeIP, t.Encap}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			data.Tunnels = append(data.Tunnels, t)
		}
	}
	return nil
}

func registerTunnelMetrics(m map[string]*prometheus.Desc) {
	// local_ip is part of the label set because a multi-TEP transport node
	// emits one tunnel per (source TEP, remote TEP) pair; without local_ip
	// the metric collides for every pair sharing the same remote TEP.
	labels := []string{NSXV3_MANAGER_HOSTNAME, "local_node_id", "local_node_name", "local_ip", "remote_node_id", "remote_ip", "encap"}
	m["TunnelStatus"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "tunnel", "status"),
		"NSX-T transport node tunnel status - UP=1, DEGRADED=0.5, DOWN=0, UNKNOWN=-1",
		labels, nil,
	)
	m["TunnelBFDDiagnosticCode"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "tunnel", "bfd_diagnostic_code"),
		"NSX-T tunnel BFD diagnostic code (0 = no diagnostic, non-zero = peer/path issue, see RFC 5880)",
		labels, nil,
	)
}

func (e *Exporter) emitTunnelMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, t := range data.Tunnels {
		ch <- prometheus.MustNewConstMetric(
			e.APIMetrics["TunnelStatus"],
			prometheus.GaugeValue, t.Status,
			host, t.LocalNodeID, t.LocalNodeName, t.LocalIP, t.RemoteNodeID, t.RemoteNodeIP, t.Encap,
		)
		ch <- prometheus.MustNewConstMetric(
			e.APIMetrics["TunnelBFDDiagnosticCode"],
			prometheus.GaugeValue, t.BFDDiagCode,
			host, t.LocalNodeID, t.LocalNodeName, t.LocalIP, t.RemoteNodeID, t.RemoteNodeIP, t.Encap,
		)
	}
}
