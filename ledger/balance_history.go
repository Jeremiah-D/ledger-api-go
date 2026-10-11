package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Balance-history time series (LG-59).
//
// The ledger already answers "what is the balance now" (Balance),
// "what was it at version N" (BalanceAt, journal-prefix replay), and
// "what is it at this instant with a version stamp" (Snapshot). What it
// does not answer is the time-series question operators and finance
// reconciliation actually ask: "show me this account's balance over the
// last 30 days". Replaying the journal for every such query is O(journal)
// per read; the balance-history recorder amortizes that into a
// background time series: a worker snapshots every (account, currency)
// balance on a ticker and appends one JSON row per key to per-day JSONL
// files, so a history query is a bounded file scan.
//
// Layout, under the configured directory:
//
//	balance-history-2006-01-02.jsonl   one file per UTC day, append-only
//
// Each line is one BalanceHistoryPoint:
//
//	{"account":"cash","currency":"USD","balance_cents":12500,
//	 "version":42,"at":"2026-10-11T07:00:00.123456789Z"}
//
// The series is append-only and the recorder holds no cursor: after a
// disaster-recovery snapshot restore (see ImportSnapshot), a recorder
// started with the same directory simply keeps appending to the current
// day's file — old rows are never rewritten or truncated, so the worker
// continues the series instead of resetting it. The directory is process
// configuration (like the hold-sweep interval), not ledger state, so it
// is deliberately not part of the snapshot config record: the operator
// points the restored process at the same directory and the series
// resumes.
//
// Retention is by file modification time: every tick deletes
// balance-history-*.jsonl files older than MaxAgeMs. Only files matching
// the recorder's own name pattern are ever touched, and the file a tick
// is about to append to is never pruned before the write — pruning runs
// first so a very short retention window cannot delete rows the current
// tick just wrote.

// DefaultBalanceHistoryMaxAgeMs is the default retention window for
// balance-history files: 90 days, in milliseconds.
const DefaultBalanceHistoryMaxAgeMs = int64(90 * 24 * time.Hour / time.Millisecond)

// balanceHistoryFilePrefix is the filename prefix of the recorder's own
// files. Pruning and querying only ever touch files with this prefix.
const balanceHistoryFilePrefix = "balance-history-"

// ErrBalanceHistoryNotConfigured is returned by QueryBalanceHistory when
// no history directory was supplied. The HTTP layer treats an
// unconfigured directory as 404 fail-closed (see handleBalanceHistory).
var ErrBalanceHistoryNotConfigured = errors.New("ledger: balance history directory not configured")

// BalanceHistoryConfig configures the background balance-history
// recorder. A zero value disables nothing by itself — the zero Dir is
// what disables it (see StartBalanceHistoryRecorder).
type BalanceHistoryConfig struct {
	// Dir is the directory the per-day JSONL files land in. Empty
	// disables the recorder.
	Dir string
	// Interval is how often a snapshot tick fires. The first tick fires
	// after one full interval, not immediately.
	Interval time.Duration
	// MaxAgeMs is the retention window in milliseconds: files older
	// than now-MaxAgeMs are deleted on every tick. Non-positive means
	// DefaultBalanceHistoryMaxAgeMs.
	MaxAgeMs int64
}

// withDefaults fills a non-positive MaxAgeMs with the package default.
func (c BalanceHistoryConfig) withDefaults() BalanceHistoryConfig {
	if c.MaxAgeMs <= 0 {
		c.MaxAgeMs = DefaultBalanceHistoryMaxAgeMs
	}
	return c
}

// MaxAge reports the retention window as a duration.
func (c BalanceHistoryConfig) MaxAge() time.Duration {
	return time.Duration(c.withDefaults().MaxAgeMs) * time.Millisecond
}

// BalanceHistoryPoint is one (account, currency) balance sample: the
// balance the ledger reported at At, tagged with the journal version the
// sample was taken at so a row can be cross-checked against BalanceAt.
type BalanceHistoryPoint struct {
	Account      AccountID `json:"account"`
	Currency     string    `json:"currency"`
	BalanceCents int64     `json:"balance_cents"`
	Version      uint64    `json:"version"`
	// At is the sample time, UTC.
	At time.Time `json:"at"`
}

// SnapshotBalances captures one BalanceHistoryPoint per (account,
// currency) key the ledger knows, sorted by (account, currency) for
// deterministic output. It takes only the read lock and writes nothing
// to the books — no journal rows, no version bump: the series samples the
// ledger, it is not part of it.
func (l *Ledger) SnapshotBalances() []BalanceHistoryPoint {
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := time.Now().UTC()
	points := make([]BalanceHistoryPoint, 0, len(l.balances))
	for k, b := range l.balances {
		points = append(points, BalanceHistoryPoint{
			Account:      k.account,
			Currency:     k.currency,
			BalanceCents: b,
			Version:      l.version,
			At:           now,
		})
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].Account != points[j].Account {
			return points[i].Account < points[j].Account
		}
		return points[i].Currency < points[j].Currency
	})
	return points
}

// balanceHistoryFilename names the per-day file for a UTC day.
func balanceHistoryFilename(day time.Time) string {
	return fmt.Sprintf("%s%s.jsonl", balanceHistoryFilePrefix, day.UTC().Format("2006-01-02"))
}

// balanceHistoryDay parses the UTC day back out of a recorder filename.
// It reports false for names that are not ours, so pruning and querying
// never act on foreign files.
func balanceHistoryDay(name string) (time.Time, bool) {
	rest := strings.TrimPrefix(name, balanceHistoryFilePrefix)
	if rest == name || !strings.HasSuffix(rest, ".jsonl") {
		return time.Time{}, false
	}
	day, err := time.Parse("2006-01-02", strings.TrimSuffix(rest, ".jsonl"))
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// WriteBalanceHistory appends points to the per-day JSONL files under
// dir, creating the directory (and the day files) as needed. Each tick's
// rows go out in a single buffered write per day-file followed by one
// fsync, so a reader racing a tick sees either the whole tick or — at
// worst — a torn final line, which the query path tolerates (see
// readBalanceHistoryFile). Points with a zero At are stamped with the
// write time. Writing zero points still ensures the directory exists but
// creates no files.
func WriteBalanceHistory(dir string, points []BalanceHistoryPoint) error {
	if dir == "" {
		return ErrBalanceHistoryNotConfigured
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ledger: balance history: create dir: %w", err)
	}
	if len(points) == 0 {
		return nil
	}
	now := time.Now().UTC()
	// Group rows by UTC day; a tick's points share At, but the writer
	// stays correct for mixed input (backfill, tests).
	byDay := make(map[string][]BalanceHistoryPoint)
	for _, p := range points {
		at := p.At
		if at.IsZero() {
			at = now
			p.At = at
		}
		byDay[at.UTC().Format("2006-01-02")] = append(byDay[at.UTC().Format("2006-01-02")], p)
	}
	for day, ps := range byDay {
		var sb strings.Builder
		enc := json.NewEncoder(&sb)
		for _, p := range ps {
			if err := enc.Encode(p); err != nil {
				return fmt.Errorf("ledger: balance history: encode: %w", err)
			}
		}
		f, err := os.OpenFile(filepath.Join(dir, balanceHistoryFilePrefix+day+".jsonl"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("ledger: balance history: open: %w", err)
		}
		_, werr := f.WriteString(sb.String())
		serr := f.Sync()
		cerr := f.Close()
		switch {
		case werr != nil:
			return fmt.Errorf("ledger: balance history: write: %w", werr)
		case serr != nil:
			return fmt.Errorf("ledger: balance history: fsync: %w", serr)
		case cerr != nil:
			return fmt.Errorf("ledger: balance history: close: %w", cerr)
		}
	}
	return nil
}

// PruneBalanceHistory deletes the recorder's own files under dir whose
// modification time is older than now-maxAge, and returns how many files
// were removed. Files that do not match the recorder's name pattern are
// never touched; a missing directory prunes nothing. Modification time
// (not the date in the name) is the retention clock, so a restored
// process keeps the rows that are actually fresh instead of re-deriving
// age from filenames.
func PruneBalanceHistory(dir string, maxAge time.Duration) (int, error) {
	if dir == "" {
		return 0, ErrBalanceHistoryNotConfigured
	}
	matches, err := filepath.Glob(filepath.Join(dir, balanceHistoryFilePrefix+"*.jsonl"))
	if err != nil {
		return 0, fmt.Errorf("ledger: balance history: list: %w", err)
	}
	if maxAge < 0 {
		maxAge = 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, m := range matches {
		if _, ok := balanceHistoryDay(filepath.Base(m)); !ok {
			continue
		}
		fi, err := os.Stat(m)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("ledger: balance history: stat: %w", err)
		}
		if fi.ModTime().Before(cutoff) {
			if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
				return removed, fmt.Errorf("ledger: balance history: remove: %w", err)
			}
			removed++
		}
	}
	return removed, nil
}

// readBalanceHistoryFile parses one day-file into points. A corrupt line
// in the middle of the file is data corruption and fails the read —
// except for the final line of a file that does not end in a newline,
// which is the known torn tail of a tick racing this read (the writer
// emits one buffered write per tick; see WriteBalanceHistory). That tail
// is skipped instead of failing the whole query: the next tick completes
// it or the next read sees it whole.
func readBalanceHistoryFile(path string) ([]BalanceHistoryPoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ledger: balance history: read %s: %w", filepath.Base(path), err)
	}
	var out []BalanceHistoryPoint
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ledger: balance history: scan %s: %w", filepath.Base(path), err)
	}
	trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
	for i, raw := range lines {
		if len(raw) == 0 {
			continue
		}
		var p BalanceHistoryPoint
		if err := json.Unmarshal(raw, &p); err != nil {
			last := i == len(lines)-1
			if last && !trailingNewline {
				// Torn tail of a racing tick; skip it.
				continue
			}
			return nil, fmt.Errorf("ledger: balance history: corrupt line %d in %s: %w",
				i+1, filepath.Base(path), err)
		}
		out = append(out, p)
	}
	return out, nil
}

// QueryBalanceHistory reads the time series for one (account, currency)
// over [since, until]. A zero since reads from the oldest file; a zero
// until reads through today. Files are selected by the UTC day in their
// name, so a narrow window never scans the whole retention span; rows
// are then filtered by exact instant. The returned points are ordered
// by (At, Version). An empty account, an invalid currency, or an empty
// dir is an error; a missing directory simply yields no points.
func QueryBalanceHistory(dir string, account AccountID, currency string, since, until time.Time) ([]BalanceHistoryPoint, error) {
	if dir == "" {
		return nil, ErrBalanceHistoryNotConfigured
	}
	if account == "" {
		return nil, errors.New("ledger: balance history: account is required")
	}
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, balanceHistoryFilePrefix+"*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("ledger: balance history: list: %w", err)
	}
	since = since.UTC()
	until = until.UTC()
	type datedFile struct {
		path string
		day  time.Time
	}
	var files []datedFile
	for _, m := range matches {
		day, ok := balanceHistoryDay(filepath.Base(m))
		if !ok {
			continue
		}
		if !since.IsZero() && day.Before(dayStart(since)) {
			continue
		}
		if !until.IsZero() && day.After(dayStart(until)) {
			continue
		}
		files = append(files, datedFile{path: m, day: day})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].day.Before(files[j].day) })

	out := []BalanceHistoryPoint{}
	for _, f := range files {
		points, err := readBalanceHistoryFile(f.path)
		if err != nil {
			return nil, err
		}
		for _, p := range points {
			if p.Account != account || p.Currency != cur {
				continue
			}
			at := p.At.UTC()
			if !since.IsZero() && at.Before(since) {
				continue
			}
			if !until.IsZero() && at.After(until) {
				continue
			}
			p.At = at
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// dayStart truncates t to its UTC calendar day.
func dayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// BalanceHistoryRecorder is the background time-series worker: one
// goroutine that snapshots every (account, currency) balance and appends
// the rows to the configured directory on a ticker, pruning files past
// the retention window, until its context is cancelled or Stop is
// called. Like the hold/health/schedule sweepers it shares the server
// context, fires its first tick after one full interval, and is disabled
// (nil) on an empty directory or a non-positive interval.
//
// A failed tick (disk error) is reported through onTick and never kills
// the worker: the next tick retries, so a transient disk stall costs one
// sample, not the series.
type BalanceHistoryRecorder struct {
	l      *Ledger
	cfg    BalanceHistoryConfig
	onTick func(points int, err error)

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	ticks       atomic.Uint64
	pointsTotal atomic.Uint64
}

// StartBalanceHistoryRecorder launches the background recorder. After
// each tick it invokes onTick (when non-nil) with the number of rows
// written and any error, so the caller can feed its own metrics — the
// HTTP server wires successful ticks to
// ledger_balance_history_snapshots_total and logs failures.
//
// An empty Dir or a non-positive Interval disables the recorder: it
// returns nil and starts nothing, so callers can pass a configured
// struct straight through without branching.
func StartBalanceHistoryRecorder(ctx context.Context, l *Ledger, cfg BalanceHistoryConfig, onTick func(points int, err error)) *BalanceHistoryRecorder {
	if cfg.Dir == "" || cfg.Interval <= 0 {
		return nil
	}
	cfg = cfg.withDefaults()
	r := &BalanceHistoryRecorder{
		l:      l,
		cfg:    cfg,
		onTick: onTick,
		done:   make(chan struct{}),
	}
	r.wg.Add(1)
	go r.run(ctx)
	return r
}

func (r *BalanceHistoryRecorder) run(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

// tick captures one full (account, currency) snapshot, appends it, then
// prunes files past the retention window. The ledger lock is held only
// for the capture; disk I/O happens unlocked so a slow disk never blocks
// posting.
func (r *BalanceHistoryRecorder) tick() {
	points := r.l.SnapshotBalances()
	var tickErr error
	if err := WriteBalanceHistory(r.cfg.Dir, points); err != nil {
		tickErr = err
	} else if _, err := PruneBalanceHistory(r.cfg.Dir, r.cfg.MaxAge()); err != nil {
		tickErr = err
	} else {
		r.ticks.Add(1)
		r.pointsTotal.Add(uint64(len(points)))
	}
	if r.onTick != nil {
		r.onTick(len(points), tickErr)
	}
}

// Stop halts the recorder and waits for its goroutine to exit. It is
// idempotent and safe to call alongside context cancellation — whichever
// fires first wins, the other is a no-op.
func (r *BalanceHistoryRecorder) Stop() {
	r.stopOnce.Do(func() { close(r.done) })
	r.wg.Wait()
}

// Ticks reports how many snapshot ticks completed successfully.
// PointsTotal reports the total rows written across all ticks.
func (r *BalanceHistoryRecorder) Ticks() uint64       { return r.ticks.Load() }
func (r *BalanceHistoryRecorder) PointsTotal() uint64 { return r.pointsTotal.Load() }
