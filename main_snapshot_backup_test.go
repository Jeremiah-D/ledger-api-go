package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

func TestSnapshotBackupConfig(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want ledger.SnapshotBackupConfig
		ok   bool
	}{
		{
			"unset dir disables",
			map[string]string{},
			ledger.SnapshotBackupConfig{},
			false,
		},
		{
			"dir without interval disables",
			map[string]string{"LEDGER_SNAPSHOT_BACKUP_DIR": "/tmp/x"},
			ledger.SnapshotBackupConfig{},
			false,
		},
		{
			"invalid interval disables",
			map[string]string{
				"LEDGER_SNAPSHOT_BACKUP_DIR":      "/tmp/x",
				"LEDGER_SNAPSHOT_BACKUP_INTERVAL": "soon",
			},
			ledger.SnapshotBackupConfig{},
			false,
		},
		{
			"full config with defaults",
			map[string]string{
				"LEDGER_SNAPSHOT_BACKUP_DIR":      "/tmp/x",
				"LEDGER_SNAPSHOT_BACKUP_INTERVAL": "1h",
			},
			ledger.SnapshotBackupConfig{
				Dir:             "/tmp/x",
				Interval:        time.Hour,
				KeepFull:        ledger.DefaultSnapshotBackupKeepFull,
				KeepIncremental: ledger.DefaultSnapshotBackupKeepIncremental,
				FullEvery:       ledger.DefaultSnapshotBackupFullEvery,
			},
			true,
		},
		{
			"custom retention",
			map[string]string{
				"LEDGER_SNAPSHOT_BACKUP_DIR":       "/tmp/x",
				"LEDGER_SNAPSHOT_BACKUP_INTERVAL":  "30m",
				"LEDGER_SNAPSHOT_BACKUP_KEEP_FULL": "3",
				"LEDGER_SNAPSHOT_BACKUP_KEEP_INCR": "12",
				"LEDGER_SNAPSHOT_BACKUP_FULL_EVERY": "4",
			},
			ledger.SnapshotBackupConfig{
				Dir:             "/tmp/x",
				Interval:        30 * time.Minute,
				KeepFull:        3,
				KeepIncremental: 12,
				FullEvery:       4,
			},
			true,
		},
		{
			"garbage retention falls back to defaults",
			map[string]string{
				"LEDGER_SNAPSHOT_BACKUP_DIR":       "/tmp/x",
				"LEDGER_SNAPSHOT_BACKUP_INTERVAL":  "30m",
				"LEDGER_SNAPSHOT_BACKUP_KEEP_FULL": "many",
			},
			ledger.SnapshotBackupConfig{
				Dir:             "/tmp/x",
				Interval:        30 * time.Minute,
				KeepFull:        ledger.DefaultSnapshotBackupKeepFull,
				KeepIncremental: ledger.DefaultSnapshotBackupKeepIncremental,
				FullEvery:       ledger.DefaultSnapshotBackupFullEvery,
			},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{
				"LEDGER_SNAPSHOT_BACKUP_DIR",
				"LEDGER_SNAPSHOT_BACKUP_INTERVAL",
				"LEDGER_SNAPSHOT_BACKUP_KEEP_FULL",
				"LEDGER_SNAPSHOT_BACKUP_KEEP_INCR",
				"LEDGER_SNAPSHOT_BACKUP_FULL_EVERY",
			} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, ok := snapshotBackupConfig()
			if ok != tc.ok {
				t.Fatalf("snapshotBackupConfig() ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got != tc.want {
				t.Fatalf("snapshotBackupConfig() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSnapshotBackupMetricsExposed wires the worker callback into the
// server metrics and checks the Prometheus exposition names.
func TestSnapshotBackupMetricsExposed(t *testing.T) {
	srv := newServer(ledger.New())
	srv.metrics.SnapshotBackupsTotal.Add(2)
	srv.metrics.SnapshotBackupsFailed.Add(1)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"ledger_snapshot_backups_total 2\n",
		"ledger_snapshot_backups_failed_total 1\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
}
