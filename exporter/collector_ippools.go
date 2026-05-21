// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// Nsxv3IPPoolData captures the allocation state of one IP pool. The Source
// label distinguishes management-plane (MP) pools used for TEP/host overlay
// from policy pools used by segments/VMs.
type Nsxv3IPPoolData struct {
	ID          string
	DisplayName string
	Source      string // "POLICY" or "MP" (TEP pools live under MP)
	Total       float64
	Allocated   float64
	Free        float64
}

// MP pool usage shape: /api/v1/pools/ip-pools/<id> embeds pool_usage.
type mpPool struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	PoolUsage   struct {
		TotalIDs     float64 `json:"total_ids"`
		AllocatedIDs float64 `json:"allocated_ids"`
		FreeIDs      float64 `json:"free_ids"`
	} `json:"pool_usage"`
}

type mpPoolList struct {
	Results []mpPool `json:"results"`
	Cursor  string   `json:"cursor"`
}

// Policy pool list returns id + display_name only; usage lives at a dedicated
// /ip-pool-usage subresource.
type policyPoolUsage struct {
	PoolBlockTotalIDs     float64 `json:"pool_block_total_ids"`
	PoolBlockAllocatedIDs float64 `json:"pool_block_allocated_ids"`
	PoolBlockFreeIDs      float64 `json:"pool_block_free_ids"`
	// Some 4.x responses use these names instead.
	TotalIDs     float64 `json:"total_ids"`
	AllocatedIDs float64 `json:"allocated_ids"`
	FreeIDs      float64 `json:"free_ids"`
}

func collectIPPools(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	var pools []Nsxv3IPPoolData

	// Management-plane pools (includes TEP pools).
	mpPools, err := listMPIPPools(ctx, client)
	if err != nil {
		log.Warnf("MP IP pool list failed: %v", err)
	}
	for _, p := range mpPools {
		pools = append(pools, Nsxv3IPPoolData{
			ID:          p.ID,
			DisplayName: p.DisplayName,
			Source:      "MP",
			Total:       p.PoolUsage.TotalIDs,
			Allocated:   p.PoolUsage.AllocatedIDs,
			Free:        p.PoolUsage.FreeIDs,
		})
	}

	// Policy pools (segment/VM IP allocations).
	policyPools, err := listPolicyResources(ctx, client, "/policy/api/v1/infra/ip-pools")
	if err != nil {
		log.Warnf("policy IP pool list failed: %v", err)
	}
	if len(policyPools) > 0 {
		results := make([]Nsxv3IPPoolData, len(policyPools))
		var wg sync.WaitGroup
		for i, p := range policyPools {
			wg.Add(1)
			go func(idx int, id, name string) {
				defer wg.Done()
				var u policyPoolUsage
				if err := client.Get(ctx, "/policy/api/v1/infra/ip-pools/"+id+"/ip-pool-usage", &u); err != nil {
					log.Warnf("policy pool usage fetch failed for %s: %v", id, err)
					results[idx] = Nsxv3IPPoolData{ID: id, DisplayName: name, Source: "POLICY"}
					return
				}
				total := u.PoolBlockTotalIDs
				allocated := u.PoolBlockAllocatedIDs
				free := u.PoolBlockFreeIDs
				if total == 0 && u.TotalIDs > 0 {
					total = u.TotalIDs
					allocated = u.AllocatedIDs
					free = u.FreeIDs
				}
				results[idx] = Nsxv3IPPoolData{
					ID: id, DisplayName: name, Source: "POLICY",
					Total: total, Allocated: allocated, Free: free,
				}
			}(i, p.ID, p.DisplayName)
		}
		wg.Wait()
		pools = append(pools, results...)
	}

	data.IPPools = pools
	return nil
}

func listMPIPPools(ctx context.Context, client *Nsxv3Client) ([]mpPool, error) {
	var all []mpPool
	cursor := ""
	for {
		p := "/api/v1/pools/ip-pools?page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp mpPoolList
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

func registerIPPoolMetrics(m map[string]*prometheus.Desc) {
	labels := []string{NSXV3_MANAGER_HOSTNAME, "pool_id", "pool_name", "source"}
	m["IPPoolTotal"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "ip_pool", "total"),
		"NSX-T IP pool total addresses. source=MP for TEP/host-overlay pools, source=POLICY for segment pools",
		labels, nil,
	)
	m["IPPoolAllocated"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "ip_pool", "allocated"),
		"NSX-T IP pool allocated addresses",
		labels, nil,
	)
	m["IPPoolFree"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "ip_pool", "free"),
		"NSX-T IP pool free addresses",
		labels, nil,
	)
	m["IPPoolUsageRatio"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "ip_pool", "usage_ratio"),
		"NSX-T IP pool allocated/total. >0.85 = capacity warning, >0.95 = imminent exhaustion",
		labels, nil,
	)
}

func (e *Exporter) emitIPPoolMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, p := range data.IPPools {
		lvals := []string{host, p.ID, p.DisplayName, p.Source}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["IPPoolTotal"], prometheus.GaugeValue, p.Total, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["IPPoolAllocated"], prometheus.GaugeValue, p.Allocated, lvals...)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["IPPoolFree"], prometheus.GaugeValue, p.Free, lvals...)
		ratio := 0.0
		if p.Total > 0 {
			ratio = p.Allocated / p.Total
		}
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["IPPoolUsageRatio"], prometheus.GaugeValue, ratio, lvals...)
	}
}
