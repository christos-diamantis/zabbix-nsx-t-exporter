// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Nsxv3CertificateData captures expiry data for one certificate registered
// with NSX-T trust management.
type Nsxv3CertificateData struct {
	ID           string
	DisplayName  string
	ResourceType string
	UsedBy       string // comma-joined service/node IDs referencing this cert
	NotAfterUnix float64 // absolute epoch seconds; time at which the cert expires
}

type trustCertEntry struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	ResourceType string `json:"resource_type"`

	// Details is only populated when the request includes ?details=true.
	// Without that flag NSX-T returns just the PEM and metadata; with it
	// the manager parses the X.509 chain and exposes not_after for each
	// cert in the chain. details[0] is the leaf certificate.
	Details []struct {
		NotAfter float64 `json:"not_after"` // epoch milliseconds
	} `json:"details"`

	UsedBy []struct {
		ServiceTypes []string `json:"service_types"`
		NodeID       string   `json:"node_id"`
	} `json:"used_by"`
}

type trustCertList struct {
	Results []trustCertEntry `json:"results"`
	Cursor  string           `json:"cursor"`
}

// leafNotAfterUnix returns the leaf cert's not_after in epoch seconds.
// Returns 0 when details[] is empty (cert with no parseable X.509 chain).
func (c *trustCertEntry) leafNotAfterUnix() float64 {
	if len(c.Details) == 0 {
		return 0
	}
	return c.Details[0].NotAfter / 1000.0 // ms -> seconds
}

func collectCertificates(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	var out []Nsxv3CertificateData
	cursor := ""
	for {
		// ?details=true forces NSX-T to parse the X.509 chain server-side
		// and populate details[].not_after. Without it the response carries
		// only pem_encoded and metadata, with no expiry fields.
		p := "/api/v1/trust-management/certificates?details=true&page_size=200"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		var resp trustCertList
		if err := client.Get(ctx, p, &resp); err != nil {
			return err
		}
		for _, c := range resp.Results {
			usedByParts := []string{}
			for _, u := range c.UsedBy {
				usedByParts = append(usedByParts, u.ServiceTypes...)
				if u.NodeID != "" {
					usedByParts = append(usedByParts, u.NodeID)
				}
			}
			out = append(out, Nsxv3CertificateData{
				ID:           c.ID,
				DisplayName:  c.DisplayName,
				ResourceType: c.ResourceType,
				UsedBy:       strings.Join(usedByParts, ","),
				NotAfterUnix: c.leafNotAfterUnix(),
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
	m["CertificateNotAfter"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "certificate", "not_after_timestamp"),
		"NSX-T certificate expiry as a UNIX timestamp (epoch seconds). Compare against now() to derive remaining lifetime",
		labels, nil,
	)
}

func (e *Exporter) emitCertificateMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for _, c := range data.Certificates {
		ch <- prometheus.MustNewConstMetric(
			e.APIMetrics["CertificateNotAfter"],
			prometheus.GaugeValue, c.NotAfterUnix,
			host, c.ID, c.DisplayName, c.ResourceType, c.UsedBy,
		)
	}
}
