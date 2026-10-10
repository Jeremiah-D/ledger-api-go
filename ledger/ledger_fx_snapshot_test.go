package ledger

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FX rate-table snapshot versioning (LG-45): every rate-table mutation
// bumps the version and persists a full copy of the table; FX transfers
// pin the version they converted at; Reconcile reports continuity; the
// history survives ExportSnapshot/ImportSnapshot.

func TestFXSnapshotVersionBumps(t *testing.T) {
	l := New()
	if v := l.FXRateSnapshotVersion(); v != 0 {
		t.Fatalf("fresh ledger snapshot version = %d, want 0", v)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if v := l.FXRateSnapshotVersion(); v != 1 {
		t.Fatalf("after SetFXRate version = %d, want 1", v)
	}
	rate, err := ParseFXRateDecimal("1.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SetFXRateRat("USD", "EUR", rate, 0); err != nil {
		t.Fatalf("SetFXRateRat: %v", err)
	}
	if v := l.FXRateSnapshotVersion(); v != 2 {
		t.Fatalf("after SetFXRateRat version = %d, want 2", v)
	}
	removed, err := l.RemoveFXRate("USD", "EUR")
	if err != nil || !removed {
		t.Fatalf("RemoveFXRate = %v,%v; want true,nil", removed, err)
	}
	if v := l.FXRateSnapshotVersion(); v != 3 {
		t.Fatalf("after RemoveFXRate version = %d, want 3", v)
	}
	// Removing a missing pair changes nothing: no version bump.
	removed, err = l.RemoveFXRate("USD", "EUR")
	if err != nil || removed {
		t.Fatalf("RemoveFXRate missing = %v,%v; want false,nil", removed, err)
	}
	if v := l.FXRateSnapshotVersion(); v != 3 {
		t.Fatalf("after no-op remove version = %d, want 3", v)
	}
	// A rejected rate never touches the table: no version bump.
	if err := l.SetFXRate("USD", "USD", 1, 1); err == nil {
		t.Fatal("same-currency SetFXRate must fail")
	}
	if v := l.FXRateSnapshotVersion(); v != 3 {
		t.Fatalf("after rejected SetFXRate version = %d, want 3", v)
	}
}

func TestFXSnapshotGenesis(t *testing.T) {
	l := New(
		WithFXAccount("fx-pnl"),
		WithFXRateOption("USD", "EUR", 108, 100),
		WithFXRateOption("USD", "CNY", 720, 100),
	)
	if v := l.FXRateSnapshotVersion(); v != 1 {
		t.Fatalf("genesis snapshot version = %d, want 1", v)
	}
	snap, ok := l.FXRateTableSnapshot(1)
	if !ok {
		t.Fatal("genesis snapshot missing")
	}
	if snap.Change != "genesis" {
		t.Errorf("genesis snapshot change = %q, want %q", snap.Change, "genesis")
	}
	if len(snap.Rates) != 2 {
		t.Fatalf("genesis snapshot rates = %d, want 2", len(snap.Rates))
	}
	// Sorted by (from, to): CNY before EUR.
	if snap.Rates[0].ToCurrency != "CNY" || snap.Rates[1].ToCurrency != "EUR" {
		t.Errorf("genesis snapshot not sorted: %+v", snap.Rates)
	}
	if _, ok := l.FXRateTableSnapshot(2); ok {
		t.Error("snapshot 2 must not exist yet")
	}
}

func TestFXSnapshotHistoryReproducible(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 108, 100) // version 1
	mustSetRate(t, l, "USD", "EUR", 110, 100) // version 2
	mustSetRate(t, l, "USD", "CNY", 720, 100) // version 3

	s1, ok := l.FXRateTableSnapshot(1)
	if !ok {
		t.Fatal("snapshot 1 missing")
	}
	if len(s1.Rates) != 1 || s1.Rates[0].Num != 108 || s1.Rates[0].Den != 100 {
		t.Errorf("snapshot 1 must pin the old 108/100 rate: %+v", s1.Rates)
	}
	s2, ok := l.FXRateTableSnapshot(2)
	if !ok {
		t.Fatal("snapshot 2 missing")
	}
	if len(s2.Rates) != 1 || s2.Rates[0].Num != 110 {
		t.Errorf("snapshot 2 must carry the 110/100 rate: %+v", s2.Rates)
	}
	s3, ok := l.FXRateTableSnapshot(3)
	if !ok || len(s3.Rates) != 2 {
		t.Fatalf("snapshot 3 = %+v,%v; want 2 rates", s3, ok)
	}
	// Snapshots are copies: mutating the returned value must not affect
	// the stored history.
	s1.Rates[0].Num = 999
	again, _ := l.FXRateTableSnapshot(1)
	if again.Rates[0].Num != 108 {
		t.Error("returned snapshot is not a copy: mutation leaked into stored history")
	}
}

func TestFXTransferRecordsSnapshotVersion(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100) // snapshot version 1

	rcpt1, err := l.PostTransfer(Transfer{
		ID: "fx1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer fx1: %v", err)
	}
	if rcpt1.FX == nil || rcpt1.FX.RateSnapshotVersion != 1 {
		t.Fatalf("fx1 receipt snapshot version = %+v, want 1", rcpt1.FX)
	}

	// The rate changes; the new transfer pins the new version, while the
	// old receipt's version still reproduces its own table.
	mustSetRate(t, l, "USD", "EUR", 110, 100) // snapshot version 2
	rcpt2, err := l.PostTransfer(Transfer{
		ID: "fx2", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer fx2: %v", err)
	}
	if rcpt2.FX == nil || rcpt2.FX.RateSnapshotVersion != 2 {
		t.Fatalf("fx2 receipt snapshot version = %+v, want 2", rcpt2.FX)
	}
	if rcpt2.FX.ConvertedCents != 11000 {
		t.Errorf("fx2 converted = %d, want 11000", rcpt2.FX.ConvertedCents)
	}
	old, ok := l.FXRateTableSnapshot(rcpt1.FX.RateSnapshotVersion)
	if !ok {
		t.Fatal("historical snapshot for fx1 missing")
	}
	if len(old.Rates) != 1 || old.Rates[0].Num != 108 || old.Rates[0].Den != 100 {
		t.Errorf("fx1's snapshot must still show 108/100: %+v", old.Rates)
	}
	// Removing the rate afterwards does not erase history either.
	if _, err := l.RemoveFXRate("USD", "EUR"); err != nil {
		t.Fatal(err)
	}
	if old, ok = l.FXRateTableSnapshot(rcpt1.FX.RateSnapshotVersion); !ok || old.Rates[0].Num != 108 {
		t.Error("rate removal must not erase the snapshot history")
	}
}

func TestFXSnapshotAuditEvent(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	if _, err := l.RemoveFXRate("USD", "EUR"); err != nil {
		t.Fatal(err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, _, err := ReadAuditLog(al.Dir(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	var changes []AuditEvent
	for _, ev := range events {
		if ev.Op == "fx_rate_change" {
			changes = append(changes, ev)
		}
	}
	if len(changes) != 2 {
		t.Fatalf("fx_rate_change events = %d, want 2 (set + remove)", len(changes))
	}
	if changes[0].Details["change"] != "set USD->EUR 108/100" {
		t.Errorf("first change = %v, want set note", changes[0].Details["change"])
	}
	if changes[0].Details["fx_snapshot_version"] != float64(1) {
		t.Errorf("first change version = %v, want 1", changes[0].Details["fx_snapshot_version"])
	}
	if changes[1].Details["change"] != "remove USD->EUR" {
		t.Errorf("second change = %v, want remove note", changes[1].Details["change"])
	}
	// Structural change: flat version bracket, like freeze.
	if changes[0].VersionBefore != changes[0].VersionAfter {
		t.Error("fx_rate_change must not bump the ledger version")
	}
}

func TestReconcileFXVersionContinuity(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustSetRate(t, l, "USD", "CNY", 720, 100)
	rep := l.Reconcile(time.Now())
	fxv := rep.FXRateVersions
	if fxv.CurrentVersion != 2 || fxv.TotalSnapshots != 2 {
		t.Errorf("FXRateVersions = %+v, want current=2 total=2", fxv)
	}
	if !fxv.ContinuityOK || fxv.ContinuityError != "" {
		t.Errorf("continuity must hold: %+v", fxv)
	}
	// A ledger that never touched FX reports version 0, still OK.
	l2 := New()
	rep2 := l2.Reconcile(time.Now())
	if rep2.FXRateVersions.CurrentVersion != 0 || !rep2.FXRateVersions.ContinuityOK {
		t.Errorf("empty FX history = %+v, want version 0 and OK", rep2.FXRateVersions)
	}
}

func TestFXSnapshotExportImportRoundTrip(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	if _, _, err := l.Post(JournalEntry{ID: "seed", DebitAccount: "payer", CreditAccount: "bank", AmountCents: 100000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	mustSetRate(t, l, "USD", "EUR", 110, 100)
	if _, err := l.PostTransfer(Transfer{
		ID: "fx1", From: "payer", To: "payee", AmountCents: 10000,
		Currency: "USD", ToCurrency: "EUR",
	}); err != nil {
		t.Fatalf("PostTransfer: %v", err)
	}

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !snapshotLedgersEqual(l, restored) {
		t.Fatal("restored ledger differs (snapshot history must round-trip)")
	}
	if restored.FXRateSnapshotVersion() != 2 {
		t.Errorf("restored version = %d, want 2", restored.FXRateSnapshotVersion())
	}
	s1, ok := restored.FXRateTableSnapshot(1)
	if !ok || len(s1.Rates) != 1 || s1.Rates[0].Num != 108 {
		t.Errorf("restored snapshot 1 = %+v,%v; want the 108/100 rate", s1, ok)
	}
	// A new transfer on the restored ledger pins version 2.
	rcpt, err := restored.PostTransfer(Transfer{
		ID: "fx2", From: "payer", To: "payee", AmountCents: 1000,
		Currency: "USD", ToCurrency: "EUR",
	})
	if err != nil {
		t.Fatalf("PostTransfer on restored: %v", err)
	}
	if rcpt.FX == nil || rcpt.FX.RateSnapshotVersion != 2 {
		t.Errorf("restored-ledger receipt snapshot version = %+v, want 2", rcpt.FX)
	}
}

// TestFXSnapshotLegacyImportRebuilds covers snapshots written before
// LG-45 (no fx_snapshot_version / fx_rate_table_snapshots fields): the
// import rebuilds version 1 from the table instead of claiming version 0
// with a populated table.
func TestFXSnapshotLegacyImportRebuilds(t *testing.T) {
	l := New(WithFXAccount("fx-pnl"))
	mustSetRate(t, l, "USD", "EUR", 108, 100)

	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Strip the LG-45 fields from the config record to simulate a
	// pre-LG-45 snapshot.
	var stripped bytes.Buffer
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("unmarshal snapshot line: %v", err)
		}
		if rec["record"] == "config" {
			delete(rec, "fx_snapshot_version")
			delete(rec, "fx_rate_table_snapshots")
			remarshaled, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			line = remarshaled
		}
		stripped.Write(line)
		stripped.WriteByte('\n')
	}
	restored, err := ImportSnapshot(&stripped)
	if err != nil {
		t.Fatalf("ImportSnapshot of legacy snapshot: %v", err)
	}
	if restored.FXRateSnapshotVersion() != 1 {
		t.Fatalf("legacy import version = %d, want rebuilt 1", restored.FXRateSnapshotVersion())
	}
	s1, ok := restored.FXRateTableSnapshot(1)
	if !ok || len(s1.Rates) != 1 || s1.Rates[0].Num != 108 {
		t.Errorf("rebuilt snapshot 1 = %+v,%v; want the imported 108/100 rate", s1, ok)
	}
	if !strings.Contains(s1.Change, "imported") {
		t.Errorf("rebuilt snapshot change = %q, want an %q note", s1.Change, "imported")
	}
}

func TestFXSnapshotImportRejectsBadHistory(t *testing.T) {
	l := New()
	mustSetRate(t, l, "USD", "EUR", 108, 100)
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatal(err)
	}
	munge := func(rec map[string]any) []byte {
		out, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cases := map[string]func(map[string]any){
		"snapshots without version": func(rec map[string]any) {
			rec["fx_snapshot_version"] = 0
		},
		"duplicate snapshot version": func(rec map[string]any) {
			snaps := rec["fx_rate_table_snapshots"].([]any)
			rec["fx_rate_table_snapshots"] = append(snaps, snaps[0])
		},
		"snapshot version out of range": func(rec map[string]any) {
			snaps := rec["fx_rate_table_snapshots"].([]any)
			snap := snaps[0].(map[string]any)
			snap["version"] = float64(99)
		},
		"bad rate inside snapshot": func(rec map[string]any) {
			snaps := rec["fx_rate_table_snapshots"].([]any)
			snap := snaps[0].(map[string]any)
			rates := snap["rates"].([]any)
			rate := rates[0].(map[string]any)
			rate["num"] = float64(0)
		},
	}
	for name, fn := range cases {
		var mangled bytes.Buffer
		for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			if rec["record"] == "config" {
				fn(rec)
				line = munge(rec)
			}
			mangled.Write(line)
			mangled.WriteByte('\n')
		}
		if _, err := ImportSnapshot(&mangled); err == nil {
			t.Errorf("%s: import must fail", name)
		}
	}
}
