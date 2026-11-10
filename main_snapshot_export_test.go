package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jeremiah-D/ledger-api-go/ledger"
)

// Full snapshot export flow through HTTP: seed entries, export the
// JSONL disaster-recovery snapshot, and verify the status, the
// content type, the meta row against the live ledger, the round-trip
// through ImportSnapshot, the repeat-export difference, the empty
// ledger, the metrics counter, the read-only version, and the
// streaming write.
//
// Note on repeat exports: the meta row carries ExportedAt
// (time.Now), so two exports are never byte-identical — they differ
// only in that row, while Version and MerkleRoot are identical. The
// repeat test below pins exactly that difference; no test here (or
// anywhere) claims repeat exports are byte-identical.

// snapshotExportMeta is the test-side mirror of the snapshot meta
// row (see ledger/snapshot.go, snapshotMetaLine).
type snapshotExportMeta struct {
	Record     string    `json:"record"`
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	Version    uint64    `json:"version"`
	EntryCount int       `json:"entry_count"`
	ChainLinks int       `json:"chain_links"`
	MerkleRoot string    `json:"merkle_root"`
}

func seedSnapshotLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l := ledger.New()
	base := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	seed := []ledger.JournalEntry{
		{ID: "snap-a1", DebitAccount: "ops", CreditAccount: "equity", AmountCents: 15000, Currency: "USD", CreatedAt: base},
		{ID: "snap-b1", DebitAccount: "ops", CreditAccount: "equity", AmountCents: 5000, Currency: "USD", CreatedAt: base.Add(30 * time.Minute), Memo: "salary"},
		{ID: "snap-c1", DebitAccount: "rent", CreditAccount: "ops", AmountCents: 2000, Currency: "USD", CreatedAt: base.Add(time.Hour)},
	}
	for _, e := range seed {
		if _, dup, err := l.Post(e); err != nil || dup {
			t.Fatalf("seed Post(%s) = dup=%v err=%v", e.ID, dup, err)
		}
	}
	return l
}

func getSnapshot(t *testing.T, raw string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(raw)
	if err != nil {
		t.Fatalf("GET %s: %v", raw, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", raw, err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func parseSnapshotMeta(t *testing.T, body string) (snapshotExportMeta, []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("snapshot is empty, want a meta row first")
	}
	var meta snapshotExportMeta
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("meta row does not parse: %v\nfirst line: %s", err, lines[0])
	}
	return meta, lines
}

func TestHTTPSnapshotExportStatusAndContentType(t *testing.T) {
	l := seedSnapshotLedger(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, hdr, body := getSnapshot(t, srv.URL+"/exports/snapshot")
	if code != http.StatusOK {
		t.Fatalf("export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/x-ndjson; charset=utf-8" {
		t.Errorf("content-type = %q, want \"application/x-ndjson; charset=utf-8\"", ct)
	}
	if body == "" {
		t.Fatal("export body is empty")
	}
	meta, lines := parseSnapshotMeta(t, body)
	if meta.Record != "meta" {
		t.Errorf("meta record = %q, want \"meta\"", meta.Record)
	}
	if meta.Format != 1 {
		t.Errorf("meta format = %d, want 1", meta.Format)
	}
	if meta.EntryCount != 3 {
		t.Errorf("meta entry_count = %d, want 3", meta.EntryCount)
	}
	if n := strings.Count(body, `"record":"entry"`); n != 3 {
		t.Errorf("got %d entry lines, want 3", n)
	}
	if len(lines) < 1+2*3 {
		t.Errorf("got %d lines, want at least %d (meta + entry/link pairs)", len(lines), 1+2*3)
	}
}

func TestHTTPSnapshotExportMetaMatchesLiveLedger(t *testing.T) {
	l := seedSnapshotLedger(t)
	wantRoot, wantVersion := l.SnapshotMerkleRoot()
	if wantVersion != l.Version() {
		t.Fatalf("SnapshotMerkleRoot version=%d, Version()=%d", wantVersion, l.Version())
	}
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	_, _, body := getSnapshot(t, srv.URL+"/exports/snapshot")
	meta, _ := parseSnapshotMeta(t, body)
	if meta.Format != 1 {
		t.Errorf("meta format = %d, want 1", meta.Format)
	}
	if meta.Version != l.Version() {
		t.Errorf("meta version = %d, want live version %d", meta.Version, l.Version())
	}
	if meta.MerkleRoot != wantRoot {
		t.Errorf("meta merkle_root = %q, want live root %q", meta.MerkleRoot, wantRoot)
	}
	if meta.EntryCount != 3 || meta.ChainLinks != 3 {
		t.Errorf("meta entry_count=%d chain_links=%d, want 3/3", meta.EntryCount, meta.ChainLinks)
	}
	if meta.ExportedAt.IsZero() {
		t.Error("meta exported_at is zero")
	}
}

func TestHTTPSnapshotExportRoundTripsThroughImport(t *testing.T) {
	l := seedSnapshotLedger(t)
	wantRoot, _ := l.SnapshotMerkleRoot()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	_, _, body := getSnapshot(t, srv.URL+"/exports/snapshot")
	restored, err := ledger.ImportSnapshot(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ImportSnapshot(exported body): %v", err)
	}
	if restored.Version() != l.Version() {
		t.Errorf("restored version = %d, want %d", restored.Version(), l.Version())
	}
	if gotRoot, _ := restored.SnapshotMerkleRoot(); gotRoot != wantRoot {
		t.Errorf("restored merkle root = %q, want %q", gotRoot, wantRoot)
	}
	// A re-export of the restored ledger must reproduce the original
	// snapshot line for line, except the meta row's ExportedAt.
	var buf strings.Builder
	if err := restored.ExportSnapshot(&buf); err != nil {
		t.Fatalf("restored ExportSnapshot: %v", err)
	}
	origMeta, origLines := parseSnapshotMeta(t, body)
	newMeta, newLines := parseSnapshotMeta(t, buf.String())
	origMeta.ExportedAt = time.Time{}
	newMeta.ExportedAt = time.Time{}
	if origMeta != newMeta {
		t.Errorf("restored meta %+v, want %+v (ignoring exported_at)", newMeta, origMeta)
	}
	if len(newLines) != len(origLines) {
		t.Fatalf("restored export has %d lines, want %d", len(newLines), len(origLines))
	}
	for i := 1; i < len(origLines); i++ {
		if newLines[i] != origLines[i] {
			t.Errorf("restored line %d differs:\n got: %s\nwant: %s", i, newLines[i], origLines[i])
		}
	}
}

func TestHTTPSnapshotExportRepeatDiffersOnlyInExportedAt(t *testing.T) {
	l := seedSnapshotLedger(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	_, _, body1 := getSnapshot(t, srv.URL+"/exports/snapshot")
	meta1, lines1 := parseSnapshotMeta(t, body1)
	// ExportedAt is wall-clock time.Now: give the clock a chance to
	// move so the difference is observable, then export again.
	time.Sleep(2 * time.Millisecond)
	_, _, body2 := getSnapshot(t, srv.URL+"/exports/snapshot")
	meta2, lines2 := parseSnapshotMeta(t, body2)

	if meta1.Version != meta2.Version {
		t.Errorf("repeat export version %d -> %d, want identical", meta1.Version, meta2.Version)
	}
	if meta1.MerkleRoot != meta2.MerkleRoot {
		t.Errorf("repeat export merkle_root %q -> %q, want identical", meta1.MerkleRoot, meta2.MerkleRoot)
	}
	if meta1.Format != meta2.Format || meta1.EntryCount != meta2.EntryCount {
		t.Errorf("repeat export meta changed beyond exported_at: %+v vs %+v", meta1, meta2)
	}
	if !meta2.ExportedAt.After(meta1.ExportedAt) {
		t.Errorf("repeat export exported_at %v not after %v", meta2.ExportedAt, meta1.ExportedAt)
	}
	if len(lines1) != len(lines2) {
		t.Fatalf("repeat export has %d lines, want %d", len(lines2), len(lines1))
	}
	for i := 1; i < len(lines1); i++ {
		if lines1[i] != lines2[i] {
			t.Errorf("repeat export line %d differs (only the meta row may differ):\nfirst:  %s\nsecond: %s", i, lines1[i], lines2[i])
		}
	}
	if body1 == body2 {
		t.Error("repeat exports are byte-identical; the meta ExportedAt row must differ")
	}
}

func TestHTTPSnapshotExportEmptyLedger(t *testing.T) {
	l := ledger.New()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	code, hdr, body := getSnapshot(t, srv.URL+"/exports/snapshot")
	if code != http.StatusOK {
		t.Fatalf("empty export: status=%d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/x-ndjson; charset=utf-8" {
		t.Errorf("empty export content-type = %q, want application/x-ndjson", ct)
	}
	meta, lines := parseSnapshotMeta(t, body)
	if meta.Format != 1 || meta.Version != 0 || meta.EntryCount != 0 {
		t.Errorf("empty meta = %+v, want format 1, version 0, entry_count 0", meta)
	}
	// Meta plus the always-written config line; no entry/link lines.
	if len(lines) != 2 {
		t.Errorf("empty export has %d lines, want 2 (meta + config)", len(lines))
	}
	if len(lines) == 2 && !strings.Contains(lines[1], `"record":"config"`) {
		t.Errorf("empty export second line = %s, want the config record", lines[1])
	}
	if strings.Contains(body, `"record":"entry"`) {
		t.Error("empty export contains entry lines")
	}
	restored, err := ledger.ImportSnapshot(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ImportSnapshot(empty export): %v", err)
	}
	if restored.Version() != 0 {
		t.Errorf("restored empty version = %d, want 0", restored.Version())
	}
}

func TestHTTPSnapshotExportMetrics(t *testing.T) {
	l := seedSnapshotLedger(t)
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getSnapshot(t, srv.URL+"/exports/snapshot")
	getSnapshot(t, srv.URL+"/exports/snapshot")
	code, _, mbody := getSnapshot(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics: status=%d, want 200", code)
	}
	if !strings.Contains(mbody, "ledger_snapshot_exports_total 2") {
		t.Errorf("metrics missing ledger_snapshot_exports_total 2")
	}
}

func TestHTTPSnapshotExportLeavesVersionUnchanged(t *testing.T) {
	l := seedSnapshotLedger(t)
	beforeVersion := l.Version()
	beforeRoot, _ := l.SnapshotMerkleRoot()
	srv := httptest.NewServer(newRouter(l))
	defer srv.Close()

	getSnapshot(t, srv.URL+"/exports/snapshot")
	if got := l.Version(); got != beforeVersion {
		t.Errorf("version after export = %d, want unchanged %d (export must be read-only)", got, beforeVersion)
	}
	if gotRoot, _ := l.SnapshotMerkleRoot(); gotRoot != beforeRoot {
		t.Errorf("merkle root after export = %q, want unchanged %q", gotRoot, beforeRoot)
	}
}

// chunkRecordingWriter records every Write call separately, so the
// test can tell whether the handler streamed the snapshot line by
// line instead of buffering the whole body into one write.
type chunkRecordingWriter struct {
	header http.Header
	chunks [][]byte
	status int
}

func (w *chunkRecordingWriter) Header() http.Header { return w.header }
func (w *chunkRecordingWriter) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	w.chunks = append(w.chunks, cp)
	return len(b), nil
}
func (w *chunkRecordingWriter) WriteHeader(status int) { w.status = status }

func TestHTTPSnapshotExportStreamsWithoutBuffering(t *testing.T) {
	l := seedSnapshotLedger(t)
	s := newServer(l)

	rec := &chunkRecordingWriter{header: http.Header{}}
	req := httptest.NewRequest(http.MethodGet, "/exports/snapshot", nil)
	s.handleSnapshotExport(rec, req)

	if rec.status != http.StatusOK {
		t.Fatalf("handler status=%d, want 200", rec.status)
	}
	if ct := rec.header.Get("Content-Type"); ct != "application/x-ndjson; charset=utf-8" {
		t.Errorf("content-type = %q, want application/x-ndjson", ct)
	}
	// The snapshot holds the read lock for its whole duration and can
	// be full-ledger size, so the handler must pass the ResponseWriter
	// straight through: one Write per encoded line, the meta line
	// first, and no single chunk holding the whole body.
	if len(rec.chunks) < 1+2*3 {
		t.Fatalf("got %d write chunks, want at least %d (one per line)", len(rec.chunks), 1+2*3)
	}
	var firstMeta snapshotExportMeta
	if err := json.Unmarshal(rec.chunks[0], &firstMeta); err != nil {
		t.Fatalf("first chunk is not the meta line: %v\nchunk: %s", err, rec.chunks[0])
	}
	if firstMeta.Record != "meta" {
		t.Fatalf("first chunk record = %q, want \"meta\"", firstMeta.Record)
	}
	// The first chunk must be only the meta line — if the handler had
	// buffered the whole snapshot, the first write would carry entry
	// lines too.
	if strings.Contains(string(rec.chunks[0]), `"record":"entry"`) {
		t.Error("first write chunk already contains entry lines: the export was buffered, not streamed")
	}
	// The last chunk must not be the meta line: the first line was
	// emitted before the last line, i.e. output flowed while the
	// export was still being produced.
	var lastMeta snapshotExportMeta
	if err := json.Unmarshal(rec.chunks[len(rec.chunks)-1], &lastMeta); err == nil && lastMeta.Record == "meta" {
		t.Error("last write chunk is the meta line: the first line was not emitted before the last")
	}
	// The concatenated chunks are still the complete snapshot.
	var sb strings.Builder
	for _, c := range rec.chunks {
		sb.Write(c)
	}
	if _, err := ledger.ImportSnapshot(strings.NewReader(sb.String())); err != nil {
		t.Fatalf("streamed chunks do not form a valid snapshot: %v", err)
	}
}
