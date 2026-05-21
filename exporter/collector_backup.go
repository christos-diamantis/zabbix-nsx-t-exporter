// SPDX-License-Identifier: Apache-2.0

package exporter

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

// Nsxv3BackupData aggregates the per-type backup state. NSX-T 4.x records
// three independent backup streams: cluster config, node config, and policy
// (inventory). All three must be tracked because a partial backup failure
// is silent on the UI but visible here.
type Nsxv3BackupData struct {
	PerType map[string]Nsxv3BackupTypeStatus
}

type Nsxv3BackupTypeStatus struct {
	LastSuccessTimestamp float64 // epoch seconds; 0 if never succeeded
	LastAttemptSuccess   float64 // 1 if last attempt succeeded, 0 otherwise, -1 if no attempts
	ConsecutiveFailures  float64
}

type backupStatus struct {
	StartTime float64 `json:"start_time"` // epoch millis
	EndTime   float64 `json:"end_time"`
	Success   bool    `json:"success"`
}

type backupHistoryResp struct {
	ClusterBackupStatuses   []backupStatus `json:"cluster_backup_statuses"`
	NodeBackupStatuses      []backupStatus `json:"node_backup_statuses"`
	InventoryBackupStatuses []backupStatus `json:"inventory_backup_statuses"`
}

func collectBackup(ctx context.Context, client *Nsxv3Client, data *Nsxv3Data) error {
	var resp backupHistoryResp
	if err := client.Get(ctx, "/api/v1/cluster/backups/history", &resp); err != nil {
		return err
	}

	data.Backup = Nsxv3BackupData{
		PerType: map[string]Nsxv3BackupTypeStatus{
			"cluster":   summarizeBackupHistory(resp.ClusterBackupStatuses),
			"node":      summarizeBackupHistory(resp.NodeBackupStatuses),
			"inventory": summarizeBackupHistory(resp.InventoryBackupStatuses),
		},
	}
	return nil
}

// summarizeBackupHistory reduces a backup history list to a status record.
// History is sorted by start_time descending in NSX-T responses; if that
// changes, the loop below still finds the latest by max start_time.
func summarizeBackupHistory(history []backupStatus) Nsxv3BackupTypeStatus {
	st := Nsxv3BackupTypeStatus{LastAttemptSuccess: -1}
	if len(history) == 0 {
		return st
	}

	// Locate the most recent attempt.
	latestIdx := 0
	for i, h := range history {
		if h.StartTime > history[latestIdx].StartTime {
			latestIdx = i
		}
	}
	last := history[latestIdx]
	if last.Success {
		st.LastAttemptSuccess = 1
		st.LastSuccessTimestamp = last.StartTime / 1000.0
	} else {
		st.LastAttemptSuccess = 0
	}

	// Walk attempts newer-to-older, counting failures until first success.
	type indexed struct {
		idx int
		t   float64
	}
	ordered := make([]indexed, len(history))
	for i, h := range history {
		ordered[i] = indexed{i, h.StartTime}
	}
	// Insertion sort descending by time (small N, history is bounded).
	for i := 1; i < len(ordered); i++ {
		j := i
		for j > 0 && ordered[j-1].t < ordered[j].t {
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
			j--
		}
	}

	var consecFail float64
	var foundSuccess bool
	for _, o := range ordered {
		h := history[o.idx]
		if h.Success {
			if !foundSuccess {
				st.LastSuccessTimestamp = h.StartTime / 1000.0
				foundSuccess = true
			}
			break
		}
		consecFail++
	}
	st.ConsecutiveFailures = consecFail
	return st
}

func registerBackupMetrics(m map[string]*prometheus.Desc) {
	labels := []string{NSXV3_MANAGER_HOSTNAME, "type"}
	m["BackupLastSuccessTimestamp"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "backup", "last_success_timestamp"),
		"NSX-T last successful backup as UNIX timestamp (0 if never succeeded). type=cluster|node|inventory",
		labels, nil,
	)
	m["BackupLastAttemptSuccess"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "backup", "last_attempt_success"),
		"NSX-T last backup attempt outcome: 1=success, 0=failure, -1=no attempts. type=cluster|node|inventory",
		labels, nil,
	)
	m["BackupConsecutiveFailures"] = prometheus.NewDesc(
		prometheus.BuildFQName("nsxv3", "backup", "consecutive_failures"),
		"NSX-T number of consecutive backup failures since last success. type=cluster|node|inventory",
		labels, nil,
	)
}

func (e *Exporter) emitBackupMetrics(host string, data *Nsxv3Data, ch chan<- prometheus.Metric) {
	for typ, st := range data.Backup.PerType {
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BackupLastSuccessTimestamp"], prometheus.GaugeValue, st.LastSuccessTimestamp, host, typ)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BackupLastAttemptSuccess"], prometheus.GaugeValue, st.LastAttemptSuccess, host, typ)
		ch <- prometheus.MustNewConstMetric(e.APIMetrics["BackupConsecutiveFailures"], prometheus.GaugeValue, st.ConsecutiveFailures, host, typ)
	}
}
