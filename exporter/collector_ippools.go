// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"errors"
	"net/url"
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
		// Pre-filter: pools whose IDs encode NCP/inventory provenance
		// (NCP IP blocks for k8s networks, vCenter VM/segment shadows)
		// never expose /ip-pool-usage. Skipping them upfront avoids
		// hundreds of 404 round-trips on every scrape.
		results := make([]Nsxv3IPPoolData, 0, len(policyPools))
		var resultsMu sync.Mutex
		var wg sync.WaitGroup
		for _, p := range policyPools {
			if isInventoryShadowPool(p.ID) {
				continue
			}
			wg.Add(1)
			go func(id, name string) {
				defer wg.Done()
				// PathEscape handles colons and other reserved characters
				// in NCP-generated pool IDs.
				usagePath := "/policy/api/v1/infra/ip-pools/" + url.PathEscape(id) + "/ip-pool-usage"
				var u policyPoolUsage
				if err := client.Get(ctx, usagePath, &u); err != nil {
					if errors.Is(err, ErrNotFound) {
						// Real pool but no usage subresource (rare; still
						// useful to log at debug for diagnostics).
						log.Debugf("policy pool %s has no /ip-pool-usage subresource (404)", id)
						return
					}
					log.Warnf("policy pool usage fetch failed for %s: %v", id, err)
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
				entry := Nsxv3IPPoolData{
					ID: id, DisplayName: name, Source: "POLICY",
					Total: total, Allocated: allocated, Free: free,
				}
				resultsMu.Lock()
				results = append(results, entry)
				resultsMu.Unlock()
			}(p.ID, p.DisplayName)
		}
		wg.Wait()
		pools = append(pools, results...)
	}

	data.IPPools = pools
	return nil
}

// isInventoryShadowPool returns true for policy IP pool IDs that NSX-T
// auto-generates for vCenter / segment realisation. These pools are
// inventory metadata, not real allocatable pools, and have no usage data.
//
// Observed prefixes from a production NSX-T 4.2 deployment:
//   - "domain-c<N>:..."    vCenter cluster shadow
//   - "vm-domain-c<N>:..." vCenter VM-attached shadow
//   - "vnet_<uuid>_<n>"    Segment-attached shadow
//
// Note: "ipp_*" prefixes are not filtered here because users sometimes
// auto-name real pools that way too. Real ipp_ pools succeed; shadow ones
// 404 and are silently skipped at debug level.
func isInventoryShadowPool(id string) bool {
	for _, prefix := range []string{"domain-c", "vm-domain-c", "vnet_"} {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			return true
		}
	}
	return false
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
