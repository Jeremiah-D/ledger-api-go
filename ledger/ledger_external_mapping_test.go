package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSetExternalMappingBasic(t *testing.T) {
	l := New()
	if err := l.SetExternalMapping("cash", "bank", "DE89370400440532013000", "primary settlement account"); err != nil {
		t.Fatalf("SetExternalMapping: %v", err)
	}
	if err := l.SetExternalMapping("cash", "psp", "MCH-8842", ""); err != nil {
		t.Fatalf("SetExternalMapping psp: %v", err)
	}
	if err := l.SetExternalMapping("equity", "bank", "DE75512108001245126199", ""); err != nil {
		t.Fatalf("SetExternalMapping equity: %v", err)
	}

	got := l.ListExternalMappings("cash")
	if len(got) != 2 {
		t.Fatalf("ListExternalMappings(cash) = %d rows, want 2", len(got))
	}
	// Sorted by (system, external_id): bank before psp.
	if got[0].System != "bank" || got[0].ExternalID != "DE89370400440532013000" || got[0].Note != "primary settlement account" {
		t.Errorf("row 0 = %+v, want bank/DE89370400440532013000", got[0])
	}
	if got[1].System != "psp" || got[1].ExternalID != "MCH-8842" {
		t.Errorf("row 1 = %+v, want psp/MCH-8842", got[1])
	}
	if got[0].Account != "cash" || got[1].Account != "cash" {
		t.Errorf("rows carry wrong account: %+v", got)
	}

	// Single-identifier resolution for exports.
	if id, ok := l.ExternalAccountID("cash", "bank"); !ok || id != "DE89370400440532013000" {
		t.Errorf("ExternalAccountID(cash, bank) = %q (%v), want DE89370400440532013000 (true)", id, ok)
	}
	if _, ok := l.ExternalAccountID("cash", "swift"); ok {
		t.Error("ExternalAccountID(cash, swift) = true, want false")
	}
	if got := l.ListExternalMappings("nobody"); len(got) != 0 {
		t.Errorf("ListExternalMappings(nobody) = %d rows, want 0", len(got))
	}
}

func TestSetExternalMappingValidation(t *testing.T) {
	l := New()
	cases := []struct {
		name       string
		account    AccountID
		system     string
		externalID string
		note       string
		want       error
	}{
		{"empty account", "", "bank", "123", "", ErrEmptyAccountID},
		{"empty system", "cash", "", "123", "", ErrInvalidExternalMappingSystem},
		{"bad system charset", "cash", "bank!", "123", "", ErrInvalidExternalMappingSystem},
		{"system too long", "cash", strings.Repeat("s", 65), "123", "", ErrInvalidExternalMappingSystem},
		{"empty external ID", "cash", "bank", "", "", ErrEmptyExternalMappingID},
		{"external ID too long", "cash", "bank", strings.Repeat("x", 129), "", ErrExternalMappingIDTooLong},
		{"note too long", "cash", "bank", "123", strings.Repeat("n", 256), ErrExternalMappingNoteTooLong},
	}
	for _, c := range cases {
		if err := l.SetExternalMapping(c.account, c.system, c.externalID, c.note); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// Boundary values are accepted: 64-char system, 128-char ID, 255-char note.
	if err := l.SetExternalMapping("cash", strings.Repeat("s", 64), strings.Repeat("x", 128), strings.Repeat("n", 255)); err != nil {
		t.Errorf("boundary values rejected: %v", err)
	}
}

func TestSetExternalMappingIdempotentNoAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetExternalMapping("cash", "bank", "123", "note"); err != nil {
		t.Fatal(err)
	}
	// Repeating the identical row is a silent no-op: no error, and no
	// second audit event.
	if err := l.SetExternalMapping("cash", "bank", "123", "note"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := l.ExternalMappingChangeCount(); got != 1 {
		t.Errorf("change count after replay = %d, want 1", got)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	n := 0
	for _, e := range events {
		if e.Op == "external_mapping_set" || e.Op == "external_mapping_removed" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("external mapping audit events = %d, want 1 (idempotent replay must stay silent)", n)
	}
}

func TestSetExternalMappingNoteUpdate(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetExternalMapping("cash", "bank", "123", "old note"); err != nil {
		t.Fatal(err)
	}
	// Changing the note is a real change: audited and counted.
	if err := l.SetExternalMapping("cash", "bank", "123", "new note"); err != nil {
		t.Fatal(err)
	}
	if got := l.ListExternalMappings("cash"); len(got) != 1 || got[0].Note != "new note" {
		t.Fatalf("ListExternalMappings = %+v, want one row with new note", got)
	}
	if got := l.ExternalMappingChangeCount(); got != 2 {
		t.Errorf("change count = %d, want 2", got)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	var sets []AuditEvent
	for _, e := range events {
		if e.Op == "external_mapping_set" {
			sets = append(sets, e)
		}
	}
	if len(sets) != 2 {
		t.Fatalf("external_mapping_set events = %d, want 2", len(sets))
	}
	if sets[1].Details["note"] != "new note" || sets[1].Details["system"] != "bank" || sets[1].Details["external_id"] != "123" {
		t.Errorf("second set event details = %v, want system/bank external_id/123 note/new note", sets[1].Details)
	}
	if sets[1].VersionBefore != sets[1].VersionAfter {
		t.Error("mapping change must not move the version brackets")
	}
}

func TestRemoveExternalMapping(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(WithAuditLog(al))
	if err := l.SetExternalMapping("cash", "bank", "123", ""); err != nil {
		t.Fatal(err)
	}
	if !l.RemoveExternalMapping("cash", "bank", "123") {
		t.Fatal("RemoveExternalMapping = false, want true")
	}
	if got := l.ListExternalMappings("cash"); len(got) != 0 {
		t.Errorf("rows after remove = %d, want 0", len(got))
	}
	if _, ok := l.ExternalAccountID("cash", "bank"); ok {
		t.Error("ExternalAccountID still resolves after remove")
	}
	if got := l.ExternalMappingChangeCount(); got != 2 {
		t.Errorf("change count = %d, want 2 (set + remove)", got)
	}
	// Removing an undeclared row is a no-op: false, no audit event, no
	// counter bump.
	if l.RemoveExternalMapping("cash", "bank", "123") {
		t.Error("second remove = true, want false")
	}
	if got := l.ExternalMappingChangeCount(); got != 2 {
		t.Errorf("change count after no-op remove = %d, want 2", got)
	}
	if err := al.Close(); err != nil {
		t.Fatal(err)
	}
	events, corrupt, err := ReadAuditLog(dir, time.Now().UTC().Format("2006-01-02"))
	if err != nil || corrupt != 0 {
		t.Fatalf("ReadAuditLog: %v corrupt=%d", err, corrupt)
	}
	var removed []AuditEvent
	for _, e := range events {
		if e.Op == "external_mapping_removed" {
			removed = append(removed, e)
		}
	}
	if len(removed) != 1 {
		t.Fatalf("external_mapping_removed events = %d, want 1", len(removed))
	}
	if removed[0].Details["system"] != "bank" || removed[0].Details["external_id"] != "123" {
		t.Errorf("remove event details = %v, want system/bank external_id/123", removed[0].Details)
	}
}

func TestExternalMappingNoVersionBump(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100}); err != nil {
		t.Fatal(err)
	}
	v := l.Version()
	if err := l.SetExternalMapping("cash", "bank", "123", ""); err != nil {
		t.Fatal(err)
	}
	if l.Version() != v {
		t.Errorf("version moved on mapping set: %d -> %d", v, l.Version())
	}
	l.RemoveExternalMapping("cash", "bank", "123")
	if l.Version() != v {
		t.Errorf("version moved on mapping remove: %d -> %d", v, l.Version())
	}
}

func TestExternalAccountIDDeterministic(t *testing.T) {
	l := New()
	// One account, several identifiers in one system: exports resolve
	// the smallest external ID, deterministically.
	for _, id := range []string{"zz-2", "aa-1", "mm-3"} {
		if err := l.SetExternalMapping("cash", "ofx", id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if id, ok := l.ExternalAccountID("cash", "ofx"); !ok || id != "aa-1" {
		t.Errorf("ExternalAccountID(cash, ofx) = %q (%v), want aa-1 (true)", id, ok)
	}
}

func TestExternalMappingSnapshotRoundTrip(t *testing.T) {
	src := New()
	if _, _, err := src.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100}); err != nil {
		t.Fatal(err)
	}
	if err := src.SetExternalMapping("cash", "bank", "DE89370400440532013000", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := src.SetExternalMapping("cash", "psp", "MCH-8842", ""); err != nil {
		t.Fatal(err)
	}
	if err := src.SetExternalMapping("equity", "ofx", "EXT-1", ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got := restored.ListExternalMappings("cash")
	if len(got) != 2 || got[0].System != "bank" || got[1].System != "psp" {
		t.Errorf("restored cash mappings = %+v, want bank+psp rows", got)
	}
	if got[0].Note != "primary" {
		t.Errorf("restored note = %q, want primary", got[0].Note)
	}
	if id, ok := restored.ExternalAccountID("equity", "ofx"); !ok || id != "EXT-1" {
		t.Errorf("restored ExternalAccountID(equity, ofx) = %q (%v), want EXT-1 (true)", id, ok)
	}
	if !snapshotLedgersEqual(src, restored) {
		t.Fatal("restored ledger differs from source")
	}
}

func TestExternalMappingIncrementalRoundTrip(t *testing.T) {
	src := New()
	base := src.Version()
	if _, _, err := src.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 100}); err != nil {
		t.Fatal(err)
	}
	if err := src.SetExternalMapping("cash", "bank", "123", "delta note"); err != nil {
		t.Fatal(err)
	}
	dst := New()
	var delta strings.Builder
	if err := src.ExportIncrementalSnapshot(&delta, base); err != nil {
		t.Fatalf("ExportIncrementalSnapshot: %v", err)
	}
	if err := dst.ImportIncrementalSnapshot(strings.NewReader(delta.String())); err != nil {
		t.Fatalf("ImportIncrementalSnapshot: %v", err)
	}
	got := dst.ListExternalMappings("cash")
	if len(got) != 1 || got[0].ExternalID != "123" || got[0].Note != "delta note" {
		t.Errorf("delta-restored mappings = %+v, want bank/123 with delta note", got)
	}
}

func TestExternalMappingSnapshotImportRejectsBadRow(t *testing.T) {
	src := New()
	if err := src.SetExternalMapping("cash", "bank", "123", ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	// Rewrite the snapshot's config line with a row that could never
	// have been created: a malformed system name must fail the import,
	// not install a bad row.
	lines := strings.Split(buf.String(), "\n")
	rewritten := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.Contains(ln, `"record":"config"`) {
			var cfg map[string]any
			if err := json.Unmarshal([]byte(ln), &cfg); err != nil {
				t.Fatalf("config line unmarshal: %v", err)
			}
			cfg["external_mappings"] = []any{
				map[string]any{"account": "cash", "system": "bank!", "external_id": "123"},
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ln = string(raw)
		}
		rewritten = append(rewritten, ln)
	}
	if _, err := ImportSnapshot(strings.NewReader(strings.Join(rewritten, "\n"))); err == nil {
		t.Fatal("ImportSnapshot accepted a snapshot with a malformed external mapping row")
	}
}

func TestExternalMappingReconcile(t *testing.T) {
	l := New()
	if err := l.SetExternalMapping("cash", "bank", "B-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.SetExternalMapping("cash", "psp", "P-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := l.SetExternalMapping("equity", "bank", "B-2", ""); err != nil {
		t.Fatal(err)
	}
	report := l.Reconcile(time.Now())
	if len(report.ExternalMappings) != 3 {
		t.Fatalf("report.ExternalMappings = %d rows, want 3", len(report.ExternalMappings))
	}
	// Sorted by (account, system, external_id).
	want := []ExternalAccountMapping{
		{Account: "cash", System: "bank", ExternalID: "B-1"},
		{Account: "cash", System: "psp", ExternalID: "P-1"},
		{Account: "equity", System: "bank", ExternalID: "B-2"},
	}
	for i, w := range want {
		g := report.ExternalMappings[i]
		if g.Account != w.Account || g.System != w.System || g.ExternalID != w.ExternalID {
			t.Errorf("report row %d = %+v, want %+v", i, g, w)
		}
	}
	// The canonical JSON the signed export signs must still serialize:
	// the shape holds no maps, only the sorted slice.
	if _, err := report.CanonicalJSON(); err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
}

func TestExportOFXUsesExternalMapping(t *testing.T) {
	l := New()
	if _, _, err := l.Post(JournalEntry{ID: "e1", DebitAccount: "cash", CreditAccount: "equity", AmountCents: 1000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetExternalMapping("cash", "ofx", "EXT-CASH-42", "bank-facing id"); err != nil {
		t.Fatal(err)
	}
	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	if !strings.Contains(string(doc), "<ACCTID>EXT-CASH-42</ACCTID>") {
		t.Errorf("exported OFX missing mapped ACCTID:\n%s", doc)
	}
	if strings.Contains(string(doc), "<ACCTID>cash</ACCTID>") {
		t.Errorf("exported OFX still names the internal account ID:\n%s", doc)
	}
	// A system other than "ofx" does not affect the statement.
	if err := l.SetExternalMapping("equity", "bank", "BANK-EQ", ""); err != nil {
		t.Fatal(err)
	}
	doc2, _, err := l.ExportOFX(OFXExportOptions{Account: "equity", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX equity: %v", err)
	}
	if !strings.Contains(string(doc2), "<ACCTID>equity</ACCTID>") {
		t.Errorf("unmapped-system export changed ACCTID:\n%s", doc2)
	}
	// Removing the mapping restores the internal ID.
	l.RemoveExternalMapping("cash", "ofx", "EXT-CASH-42")
	doc3, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX after remove: %v", err)
	}
	if !strings.Contains(string(doc3), "<ACCTID>cash</ACCTID>") {
		t.Errorf("export after remove missing internal ACCTID:\n%s", doc3)
	}
}

func TestExternalMappingOFXEscape(t *testing.T) {
	l := New()
	// The mapped identifier is XML-escaped like the internal ID was:
	// an attacker-controlled mapping row must not break the document.
	if err := l.SetExternalMapping("cash", "ofx", "A<B>&\"'", ""); err != nil {
		t.Fatal(err)
	}
	doc, _, err := l.ExportOFX(OFXExportOptions{Account: "cash", Currency: "USD"})
	if err != nil {
		t.Fatalf("ExportOFX: %v", err)
	}
	if !strings.Contains(string(doc), "<ACCTID>A&lt;B&gt;&amp;&quot;&apos;</ACCTID>") {
		t.Errorf("mapped ACCTID not XML-escaped:\n%s", doc)
	}
}
