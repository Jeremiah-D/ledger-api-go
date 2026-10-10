package ledger

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Periodic snapshot backup worker (LG-39).
//
// Disaster-recovery snapshots (LG-24 full, LG-28 incremental) are only
// useful if somebody actually takes them. SnapshotBackup is the
// in-process answer: a single goroutine that periodically exports the
// ledger to a configured backup directory, alternating full and
// incremental exports, and enforces a retention policy so the directory
// never grows without bound.
//
// Rotation policy: every FullEvery-th backup is a full snapshot, the
// rest are incremental deltas from the last successful backup's version.
// The worker keeps the newest KeepFull full backups and the newest
// KeepIncremental incremental backups; older files are deleted. Every
// backup — success or failure — is reported through the onBackup
// callback so the HTTP layer can feed its own counters
// (ledger_snapshot_backups_total / ledger_snapshot_backups_failed_total),
// and every successful backup is recorded in the structured audit log
// (op "snapshot_backup") with its kind, path, version, and byte size.
//
// Writes are atomic: the export lands in a temp file in the backup
// directory and is renamed into place only after the export (and its
// retries) succeed, so a crash or a failed export never leaves a
// half-written snapshot for a restore to trip over. Export failures are
// retried up to MaxRetries times with a short backoff before the tick is
// counted as failed.

// Backup retention and schedule defaults.
const (
	// DefaultSnapshotBackupKeepFull retains the newest 7 full snapshots:
	// a week of daily fulls when the worker runs on a daily cadence.
	DefaultSnapshotBackupKeepFull = 7
	// DefaultSnapshotBackupKeepIncremental retains the newest 24
	// incremental snapshots: a day of hourlies between fulls.
	DefaultSnapshotBackupKeepIncremental = 24
	// DefaultSnapshotBackupFullEvery takes a full snapshot every 6th
	// backup: with hourly ticks, a full every 6 hours.
	DefaultSnapshotBackupFullEvery = 6
	// DefaultSnapshotBackupMaxRetries retries a failed export 3 times
	// before the tick counts as failed.
	DefaultSnapshotBackupMaxRetries = 3
)

// SnapshotBackupConfig configures the periodic snapshot backup worker.
// The zero value is disabled: StartSnapshotBackup returns nil unless Dir
// is set and Interval is positive. Non-positive KeepFull,
// KeepIncremental, FullEvery, and MaxRetries fall back to the defaults
// above.
type SnapshotBackupConfig struct {
	// Dir is the backup directory. Files are written as
	// snapshot-full-v<version>-<utc-timestamp>.jsonl and
	// snapshot-incr-v<base>-v<version>-<utc-timestamp>.jsonl.
	Dir string
	// Interval is how often a backup tick fires. The first tick fires
	// after one full interval, not immediately.
	Interval time.Duration
	// KeepFull is how many of the newest full backups to retain.
	KeepFull int
	// KeepIncremental is how many of the newest incremental backups to
	// retain.
	KeepIncremental int
	// FullEvery takes a full snapshot every FullEvery-th backup; the
	// rest are incremental.
	FullEvery int
	// MaxRetries is how many times a failed export is retried before
	// the tick counts as failed.
	MaxRetries int
}

// withDefaults fills non-positive knobs with the package defaults.
func (c SnapshotBackupConfig) withDefaults() SnapshotBackupConfig {
	if c.KeepFull <= 0 {
		c.KeepFull = DefaultSnapshotBackupKeepFull
	}
	if c.KeepIncremental <= 0 {
		c.KeepIncremental = DefaultSnapshotBackupKeepIncremental
	}
	if c.FullEvery <= 0 {
		c.FullEvery = DefaultSnapshotBackupFullEvery
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = DefaultSnapshotBackupMaxRetries
	}
	return c
}

// SnapshotBackup is a running backup worker. Construct with
// StartSnapshotBackup; stop with Stop.
type SnapshotBackup struct {
	l        *Ledger
	cfg      SnapshotBackupConfig
	onBackup func(kind string, ok bool)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks atomic.Uint64
	// mu guards backups and lastVersion: the tick sequence and the
	// journal version the last successful backup covered.
	mu          sync.Mutex
	backups     uint64
	lastVersion uint64
}

// StartSnapshotBackup launches a background goroutine that exports a
// snapshot every cfg.Interval until ctx is cancelled or Stop is called.
// After each tick it invokes onBackup (when non-nil) with the backup
// kind ("full"/"incremental") and whether the tick succeeded, so the
// caller can feed its own metrics — the HTTP server wires this to
// ledger_snapshot_backups_total / ledger_snapshot_backups_failed_total.
//
// An empty Dir or a non-positive Interval disables the worker: it
// returns nil and starts nothing, so callers can pass a configured
// struct straight through without branching.
func StartSnapshotBackup(ctx context.Context, l *Ledger, cfg SnapshotBackupConfig, onBackup func(kind string, ok bool)) *SnapshotBackup {
	if cfg.Dir == "" || cfg.Interval <= 0 {
		return nil
	}
	cfg = cfg.withDefaults()
	sw := &SnapshotBackup{
		l:        l,
		cfg:      cfg,
		onBackup: onBackup,
		done:     make(chan struct{}),
	}
	sw.wg.Add(1)
	go sw.run(ctx)
	return sw
}

// Stop shuts the worker down and waits for the in-flight tick, if any,
// to finish. Safe to call twice; a nil worker is a no-op.
func (sw *SnapshotBackup) Stop() {
	if sw == nil {
		return
	}
	sw.stopOnce.Do(func() { close(sw.done) })
	sw.wg.Wait()
}

func (sw *SnapshotBackup) run(ctx context.Context) {
	defer sw.wg.Done()
	ticker := time.NewTicker(sw.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sw.done:
			return
		case <-ticker.C:
			kind, ok := sw.backupOnce()
			sw.ticks.Add(1)
			if sw.onBackup != nil {
				sw.onBackup(kind, ok)
			}
		}
	}
}

// backupOnce performs a single backup tick: pick full vs. incremental,
// export with retries, write atomically, enforce retention, and audit.
// It returns the backup kind and whether the tick succeeded. Exported
// for tests in the same package; the worker goroutine is the only
// production caller.
func (sw *SnapshotBackup) backupOnce() (kind string, ok bool) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.backups++
	full := (sw.backups-1)%uint64(sw.cfg.FullEvery) == 0
	kind = "incremental"
	if full {
		kind = "full"
	}
	base := sw.lastVersion

	// Export with retries. The export holds the ledger read lock for
	// its whole duration, so each attempt describes one consistent
	// point in time; a retry re-reads the (possibly advanced) ledger.
	var (
		data    []byte
		version uint64
		err     error
	)
	for attempt := 0; attempt <= sw.cfg.MaxRetries; attempt++ {
		var buf bytes.Buffer
		if full {
			err = sw.l.ExportSnapshot(&buf)
		} else {
			err = sw.l.ExportIncrementalSnapshot(&buf, base)
		}
		if err == nil {
			data = buf.Bytes()
			version = sw.l.Version()
			break
		}
		// Brief backoff between attempts; the final attempt reports
		// the failure without sleeping.
		if attempt < sw.cfg.MaxRetries {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if err != nil {
		return kind, false
	}

	name := snapshotBackupFilename(kind, base, version, time.Now().UTC())
	if err := writeFileAtomic(sw.cfg.Dir, name, data); err != nil {
		return kind, false
	}

	pruned := 0
	if full {
		pruned, _ = pruneSnapshots(sw.cfg.Dir, "full", sw.cfg.KeepFull)
	} else {
		pruned, _ = pruneSnapshots(sw.cfg.Dir, "incr", sw.cfg.KeepIncremental)
	}

	sw.lastVersion = version

	// The rotation is structural bookkeeping, not a posting: it never
	// bumps the ledger version, but it belongs in the audit trail so a
	// restore can prove which snapshot a recovery started from.
	// emitAudit requires holding l.mu (either lock suffices); the
	// exports above already released theirs.
	sw.l.mu.RLock()
	sw.l.emitAudit(AuditEvent{
		Op:            "snapshot_backup",
		Actor:         "SnapshotBackup",
		TraceID:       name,
		VersionBefore: base,
		VersionAfter:  version,
		Details: map[string]any{
			"kind":    kind,
			"path":    filepath.Join(sw.cfg.Dir, name),
			"bytes":   len(data),
			"pruned":  pruned,
		},
	})
	sw.l.mu.RUnlock()

	return kind, true
}

// snapshotBackupFilename lays out backup files so lexical order is
// chronological: the UTC timestamp is fixed-width, and versions are
// zero-padded.
func snapshotBackupFilename(kind string, base, version uint64, at time.Time) string {
	ts := at.Format("20060102T150405Z")
	if kind == "full" {
		return fmt.Sprintf("snapshot-full-v%020d-%s.jsonl", version, ts)
	}
	return fmt.Sprintf("snapshot-incr-v%020d-v%020d-%s.jsonl", base, version, ts)
}

// writeFileAtomic writes data to a temp file inside dir and renames it
// into place, so a crash mid-write never leaves a partial snapshot
// under its final name. The temp file is removed on any failure.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".snapshot-tmp-*")
	if err != nil {
		return fmt.Errorf("ledger: snapshot backup: %w", err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup: on success the rename moves it away and
	// Remove reports "not exist", which we ignore.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("ledger: snapshot backup: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ledger: snapshot backup: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("ledger: snapshot backup: %w", err)
	}
	return nil
}

// pruneSnapshots deletes all but the newest `keep` backup files of the
// given kind ("full"/"incremental"). Lexical filename order is
// chronological (see snapshotBackupFilename), so the tail of the sorted
// list is the retention set. It returns how many files were removed;
// per-file remove errors are ignored (a retry on the next tick prunes
// again) but do not stop the sweep.
func pruneSnapshots(dir, kind string, keep int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	prefix := "snapshot-" + kind + "-"
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".jsonl") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	removed := 0
	for _, n := range names[:max(0, len(names)-keep)] {
		if err := os.Remove(filepath.Join(dir, n)); err == nil {
			removed++
		}
	}
	return removed, nil
}
