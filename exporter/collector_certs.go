// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Nsxv3CertificateData captures expiry data for one certificate registered
// with NSX-T trust management.
type Nsxv3CertificateData struct {
	ID             string
	DisplayName    string
	ResourceType   string
	UsedBy         string // comma-joined service/node IDs referencing this cert
	SecondsToExpiry float64
}

type trustCertEntry struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	ResourceType string `json:"resource_type"`
	// NSX returns not_after as epoch milliseconds.
	NotAfter float64 `json:"not_after"`
	UsedBy   []struct {
		ServiceTypes []string `json:"service_types"`
		NodeID       string   `json:"node_id"`
	} `json:"used_by"`
}

type trustCertList struct {
	Results []trustCertEntry `json:"results"`
	Cursor  string           `json:"cursor"`
}

func collectCertificates(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	var out []Nsxv3CertificateData
	cursor := ""
	now := float64(time.Now().Unix())
	for {
		p := "/api/v1/trust-management/certificates?page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp trustCertList
		if err := client.Get(ctx, p, &resp); err != nil {
			return err
		}
		for _, c := range resp.Results {
			expiry := c.NotAfter / 1000.0 // ms -> seconds
			usedByParts := []string{}
			for _, u := range c.UsedBy {
				for _, st := range u.ServiceTypes {
					usedByParts = append(usedByParts, st)
				}
				if u.NodeID != "" {
					usedByParts = append(usedByParts, u.NodeID)
				}
			}
			out = append(out, Nsxv3CertificateData{
				ID:              c.ID,
				DisplayName:     c.DisplayName,
				ResourceType:    c.ResourceType,
				UsedBy:          strings.Join(usedByParts, ","),
				SecondsToExpiry: expiry - now,
			})
		}
		if resp.Cursor == "" {
			break
		}
		cursor = resp.Cursor
	}
	data.Certificates = out
	return nil
}

func registerCertificateMetrics(m map[string]*prometheus.Desc) {
	labels := []string{NSXV3_MANAGER_HOSTNAME, "cert_id", "cert_name", "resource_type", "used_by"}
	m["CertificateSecondsToExpiry"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "certificate", "seconds_to_expiry"),
		"NSX-T certificate seconds remaining until expiry (negative if already expired). Threshold: <30d = warn, <7d = critical",
		labels, nil,
	)
}

func (e *Exporter) emitCertificateMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, c := range data.Certificates {
		ch <- prometheus.MustNewConstMetric(
			e.APIMetrics["CertificateSecondsToExpiry"],
			prometheus.GaugeValue, c.SecondsToExpiry,
			host, c.ID, c.DisplayName, c.ResourceType, c.UsedBy,
		)
	}
}
