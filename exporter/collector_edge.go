// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// Nsxv3EdgeNodeData holds runtime stats for an edge transport node.
type Nsxv3EdgeNodeData struct {
	ID          string
	DisplayName string
	CPUCores    float64
	LoadAvg1m   float64
	MemTotal    float64
	MemUsed     float64
	MemCached   float64
	SwapTotal   float64
	SwapUsed    float64
	Uptime      float64
	Storage     []Nsxv3NodeStorageData
}

// transportNode is the subset of /api/v1/transport-nodes list response we care about.
type transportNode struct {
	ID                 string `json:"id"`
	DisplayName        string `json:"display_name"`
	NodeDeploymentInfo struct {
		ResourceType string `json:"resource_type"`
	} `json:"node_deployment_info"`
}

type transportNodeListResponse struct {
	Results []transportNode `json:"results"`
	Cursor  string          `json:"cursor"`
}

// transportNodeStatusResponse mirrors GET /api/v1/transport-nodes/<id>/status.
// Fields not used are omitted; unknown fields are ignored by encoding/json.
type transportNodeStatusResponse struct {
	NodeStatus struct {
		SystemStatus struct {
			CPUCores    float64        `json:"cpu_cores"`
			LoadAverage []float64      `json:"load_average"`
			MemTotal    float64        `json:"mem_total"`
			MemUsed     float64        `json:"mem_used"`
			MemCache    float64        `json:"mem_cache"`
			SwapTotal   float64        `json:"swap_total"`
			SwapUsed    float64        `json:"swap_used"`
			Uptime      float64        `json:"uptime"`
			FileSystems []fileSystemFS `json:"file_systems"`
		} `json:"system_status"`
	} `json:"node_status"`
}

type fileSystemFS struct {
	Mount string  `json:"mount"`
	Total float64 `json:"total"`
	Used  float64 `json:"used"`
}

// listTransportNodes enumerates all transport nodes, optionally filtered by
// node type ("EdgeNode" or "HostNode"; empty string returns all). Handles
// cursor-based pagination.
func listTransportNodes(ctx context.Context, client *Nsxv3Client, nodeType string) ([]transportNode, error) {
	var nodes []transportNode
	cursor := ""
	for {
		path := "/api/v1/transport-nodes?page_size=200"
		if nodeType != "" {
			path += "&node_types=" + nodeType
		}
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var resp transportNodeListResponse
		if err := client.Get(ctx, path, &resp); err != nil {
			return nil, err
		}
		for _, tn := range resp.Results {
			if nodeType == "" || tn.NodeDeploymentInfo.ResourceType == nodeType {
				nodes = append(nodes, tn)
			}
		}
		if resp.Cursor == "" {
			return nodes, nil
		}
		cursor = resp.Cursor
	}
}

// collectEdgeNodes populates data.EdgeNodes by fetching node-status for every
// edge transport node concurrently.
func collectEdgeNodes(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	edges, err := listTransportNodes(ctx, client, "EdgeNode")
	if err != nil {
		return err
	}
	if len(edges) == 0 {
		return nil
	}

	results := make([]Nsxv3EdgeNodeData, len(edges))
	var wg sync.WaitGroup
	for i, edge := range edges {
		wg.Add(1)
		go func(idx int, e transportNode) {
			defer wg.Done()
			var st transportNodeStatusResponse
			path := "/api/v1/transport-nodes/" + e.ID + "/status"
			if err := client.Get(ctx, path, &st); err != nil {
				log.Warnf("edge node-status fetch failed for %s: %v", e.ID, err)
				results[idx] = Nsxv3EdgeNodeData{ID: e.ID, DisplayName: e.DisplayName}
				return
			}
			ed := Nsxv3EdgeNodeData{
				ID:          e.ID,
				DisplayName: e.DisplayName,
				CPUCores:    st.NodeStatus.SystemStatus.CPUCores,
				MemTotal:    st.NodeStatus.SystemStatus.MemTotal,
				MemUsed:     st.NodeStatus.SystemStatus.MemUsed,
				MemCached:   st.NodeStatus.SystemStatus.MemCache,
				SwapTotal:   st.NodeStatus.SystemStatus.SwapTotal,
				SwapUsed:    st.NodeStatus.SystemStatus.SwapUsed,
				Uptime:      st.NodeStatus.SystemStatus.Uptime,
			}
			if len(st.NodeStatus.SystemStatus.LoadAverage) > 0 {
				ed.LoadAvg1m = st.NodeStatus.SystemStatus.LoadAverage[0]
			}
			for _, fs := range st.NodeStatus.SystemStatus.FileSystems {
				ed.Storage = append(ed.Storage, Nsxv3NodeStorageData{
					filesystem:  fs.Mount,
					totalMetric: fs.Total,
					usedMetric:  fs.Used,
				})
			}
			results[idx] = ed
		}(i, edge)
	}
	wg.Wait()

	data.EdgeNodes = results
	return nil
}

// registerEdgeNodeMetrics adds edge-node metric descriptors to the registry.
func registerEdgeNodeMetrics(m map[string]*prometheus.Desc) {
	labels := []string{NSXV3_MANAGER_HOSTNAME, NSXV3_NODE_ID, "display_name"}
	storageLabels := []string{NSXV3_MANAGER_HOSTNAME, NSXV3_NODE_ID, "display_name", "filesystem"}

	m["EdgeNodeCPUCores"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "cpu_cores"),
		"NSX-T edge transport node CPU core count",
		labels, nil,
	)
	m["EdgeNodeLoadAverage1m"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "load_average_1m"),
		"NSX-T edge transport node 1-minute load average",
		labels, nil,
	)
	m["EdgeNodeCPUUsageRatio"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "cpu_usage_ratio"),
		"NSX-T edge transport node 1-min load average normalized by CPU cores. >0.8 = sustained pressure, >1.0 = oversubscribed",
		labels, nil,
	)
	m["EdgeNodeMemoryUse"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "memory_use"),
		"NSX-T edge transport node memory used in bytes",
		labels, nil,
	)
	m["EdgeNodeMemoryTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "memory_total"),
		"NSX-T edge transport node memory total in bytes",
		labels, nil,
	)
	m["EdgeNodeMemoryUsageRatio"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "memory_usage_ratio"),
		"NSX-T edge transport node memory used / total. >0.9 = high pressure",
		labels, nil,
	)
	m["EdgeNodeSwapUse"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "swap_use"),
		"NSX-T edge transport node swap used in bytes",
		labels, nil,
	)
	m["EdgeNodeSwapTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "swap_total"),
		"NSX-T edge transport node swap total in bytes",
		labels, nil,
	)
	m["EdgeNodeUptime"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "uptime_seconds"),
		"NSX-T edge transport node uptime in seconds",
		labels, nil,
	)
	m["EdgeNodeStorageTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "storage_total"),
		"NSX-T edge transport node filesystem total in bytes",
		storageLabels, nil,
	)
	m["EdgeNodeStorageUse"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "storage_use"),
		"NSX-T edge transport node filesystem used in bytes",
		storageLabels, nil,
	)
	m["EdgeNodeStorageUsageRatio"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "edge_node", "storage_usage_ratio"),
		"NSX-T edge transport node filesystem used / total",
		storageLabels, nil,
	)
}

// emitEdgeNodeMetrics writes edge-node samples to the Prometheus channel.
func (e *Exporter) emitEdgeNodeMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, n := range data.EdgeNodes {
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeCPUCores"], prometheus.GaugeValue, n.CPUCores, host, n.ID, n.DisplayName)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeLoadAverage1m"], prometheus.GaugeValue, n.LoadAvg1m, host, n.ID, n.DisplayName)
		cpuRatio := 0.0
		if n.CPUCores > 0 {
			cpuRatio = n.LoadAvg1m / n.CPUCores
		}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeCPUUsageRatio"], prometheus.GaugeValue, cpuRatio, host, n.ID, n.DisplayName)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeMemoryUse"], prometheus.GaugeValue, n.MemUsed, host, n.ID, n.DisplayName)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeMemoryTotal"], prometheus.GaugeValue, n.MemTotal, host, n.ID, n.DisplayName)
		memRatio := 0.0
		if n.MemTotal > 0 {
			memRatio = n.MemUsed / n.MemTotal
		}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeMemoryUsageRatio"], prometheus.GaugeValue, memRatio, host, n.ID, n.DisplayName)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeSwapUse"], prometheus.GaugeValue, n.SwapUsed, host, n.ID, n.DisplayName)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeSwapTotal"], prometheus.GaugeValue, n.SwapTotal, host, n.ID, n.DisplayName)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeUptime"], prometheus.GaugeValue, n.Uptime, host, n.ID, n.DisplayName)
		for _, fs := range n.Storage {
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeStorageTotal"], prometheus.GaugeValue, fs.totalMetric, host, n.ID, n.DisplayName, fs.filesystem)
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeStorageUse"], prometheus.GaugeValue, fs.usedMetric, host, n.ID, n.DisplayName, fs.filesystem)
			ratio := 0.0
			if fs.totalMetric > 0 {
				ratio = fs.usedMetric / fs.totalMetric
			}
			ch <- prometheus.MustNewConstMetric(e.APIMetrics["EdgeNodeStorageUsageRatio"], prometheus.GaugeValue, ratio, host, n.ID, n.DisplayName, fs.filesystem)
		}
	}
}
