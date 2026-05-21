// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"

	log "github.com/sirupsen/logrus"
)

// enrichTransportNodeNames augments data.TransportNodes with display_name by
// looking each ID up against the inventory list. The upstream's
// /api/v1/transport-nodes/state endpoint only carries IDs, but dashboards and
// alerts are much more useful when the UI-friendly name is present.
//
// Cost: one paginated GET on /api/v1/transport-nodes. Cheap; runs at most once
// per scrape regardless of how many other collectors also need the inventory.
func enrichTransportNodeNames(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	if len(data.TransportNodes) == 0 {
		return nil
	}
	nodes, err := listTransportNodes(ctx, client, "")
	if err != nil {
		return err
	}
	byID := make(map[string]string, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n.DisplayName
	}
	missing := 0
	for i := range data.TransportNodes {
		if name, ok := byID[data.TransportNodes[i].ID]; ok {
			data.TransportNodes[i].DisplayName = name
		} else {
			missing++
		}
	}
	if missing > 0 {
		log.Debugf("transport node name enrichment: %d/%d had no inventory entry", missing, len(data.TransportNodes))
	}
	return nil
}
