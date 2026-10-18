package ledger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// logEventsOnDay logs n sealed events with timestamps pinned to day
// ("2006-01-02" UTC), so multi-day export fixtures are day-boundary
// independent. The caller must Close the log afterwards.
func logEventsOnDay(t *testing.T, al *AuditLog, day string, n int) {
	t.Helper()
	ts, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatalf("bad day %q: %v", day, err)
	}
	for i := 0; i < n; i++ {
		ev := sealTestEvent("post", fmt.Sprintf("%s-trace-%d", day, i), uint64(i))
		ev.Timestamp = ts.Add(time.Duration(i) * time.Second).UTC()
		if !al.Log(ev) {
			t.Fatalf("Log event %d on %s: dropped", i, day)
		}
	}
}

// sha256HexOfFile returns the hex SHA-256 of the file's bytes.
func sha256HexOfFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// verifyManifestMAC recomputes the expected HMAC-SHA256 over the
// manifest's canonical bytes (signature field absent, informational
// exported_at blanked) with the key named by sig.KeyID, per the
// audit_sign.go convention (kid || 0x00 || bytes).
func verifyManifestMAC(t *testing.T, m AuditChainExportManifest, keys []AuditSigningKey) {
	t.Helper()
	if m.Signature == nil {
		t.Fatal("manifest has no signature")
	}
	var key []byte
	for _, k := range keys {
		if k.ID == m.Signature.KeyID {
			key = k.Key
		}
	}
	if key == nil {
		t.Fatalf("no key for kid %q", m.Signature.KeyID)
	}
	canon, err := manifestSignBytes(m)
	if err != nil {
		t.Fatalf("manifestSignBytes: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(m.Signature.KeyID))
	mac.Write([]byte{0})
	mac.Write(canon)
	if got := hex.EncodeToString(mac.Sum(nil)); got != m.Signature.MAC {
		t.Fatalf("manifest MAC mismatch: got %s, manifest has %s", got, m.Signature.MAC)
	}
}

func TestExportAuditChainHappyPath(t *testing.T) {
	al := mustChainAuditLog(t)
	logTestEvents(t, al, 3)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	srcFile := auditDayFile(t, al.Dir())
	srcBefore, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	out := filepath.Join(t.TempDir(), "export")
	keys := chainTestSigningKeys(t)
	m, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: out, SigningKeys: keys})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}

	if m.Format != AuditChainExportFormat {
		t.Errorf("Format = %q, want %q", m.Format, AuditChainExportFormat)
	}
	if _, err := time.Parse(time.RFC3339Nano, m.ExportedAt); err != nil {
		t.Errorf("ExportedAt %q does not parse as RFC3339Nano: %v", m.ExportedAt, err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("Files = %d, want 1", len(m.Files))
	}
	f := m.Files[0]
	if f.Day != "2026-10-09" {
		t.Errorf("Files[0].Day = %q, want 2026-10-09", f.Day)
	}
	wantSum := sha256.Sum256(srcBefore)
	if f.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Errorf("Files[0].SHA256 mismatch: snapshot hash is not the source bytes' hash")
	}
	if f.Size != int64(len(srcBefore)) {
		t.Errorf("Files[0].Size = %d, want %d", f.Size, len(srcBefore))
	}
	// The copied file is a read-only snapshot.
	cp := filepath.Join(out, f.Name)
	st, err := os.Stat(cp)
	if err != nil {
		t.Fatalf("stat copy: %v", err)
	}
	if st.Mode().Perm()&0o222 != 0 {
		t.Errorf("copy mode = %o, want no write bits (read-only snapshot)", st.Mode().Perm())
	}
	cpBytes, err := os.ReadFile(cp)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(cpBytes) != string(srcBefore) {
		t.Error("copy bytes differ from source bytes")
	}

	rep := m.Report
	if !rep.Ok {
		t.Errorf("Report.Ok = false, want true (report: %+v)", rep)
	}
	if rep.CheckedEntries != 3 {
		t.Errorf("Report.CheckedEntries = %d, want 3", rep.CheckedEntries)
	}
	if rep.SkippedLines != 0 {
		t.Errorf("Report.SkippedLines = %d, want 0", rep.SkippedLines)
	}
	if rep.FilesChecked != 1 {
		t.Errorf("Report.FilesChecked = %d, want 1", rep.FilesChecked)
	}
	if len(rep.Head) != 64 || rep.Head == AuditGenesisPrevHash {
		t.Errorf("Report.Head = %q, want a real 64-hex seal", rep.Head)
	}

	if !m.Signed {
		t.Error("Signed = false, want true with keys configured")
	}
	if m.Signature == nil || m.Signature.KeyID != "k1" {
		t.Fatalf("Signature = %+v, want kid k1", m.Signature)
	}
	verifyManifestMAC(t, m, keys)

	// manifest.json on disk round-trips to the returned manifest.
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var onDisk AuditChainExportManifest
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if !reflect.DeepEqual(onDisk, m) {
		t.Error("manifest.json on disk differs from the returned manifest")
	}

	// The source audit dir is untouched: same bytes, no manifest.json.
	srcAfter, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if string(srcAfter) != string(srcBefore) {
		t.Error("source audit file was modified by the export")
	}
	if _, err := os.Stat(filepath.Join(al.Dir(), "manifest.json")); !os.IsNotExist(err) {
		t.Error("manifest.json was written into the source audit dir")
	}
}

func TestExportAuditChainDayRange(t *testing.T) {
	al := mustChainAuditLog(t)
	logEventsOnDay(t, al, "2026-10-09", 2)
	logEventsOnDay(t, al, "2026-10-10", 2)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	keys := chainTestSigningKeys(t)

	// A range covering only the second day: the export completes, but the
	// report is honestly not-ok — the earliest copied entry links the
	// previous day's seal, not the genesis marker.
	m, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{
		FromDay: "2026-10-10", ToDay: "2026-10-10",
		ExportDir: filepath.Join(t.TempDir(), "partial"), SigningKeys: keys,
	})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}
	if len(m.Files) != 1 || m.Files[0].Day != "2026-10-10" {
		t.Fatalf("partial export Files = %+v, want only 2026-10-10", m.Files)
	}
	if m.FromDay != "2026-10-10" || m.ToDay != "2026-10-10" {
		t.Errorf("FromDay/ToDay = %q/%q, want the requested range echoed", m.FromDay, m.ToDay)
	}
	if m.Report.Ok {
		t.Error("partial-range Report.Ok = true, want false (chain cannot link genesis)")
	}
	if m.Report.CheckedEntries != 0 {
		t.Errorf("partial-range CheckedEntries = %d, want 0 (break on the first copied entry)", m.Report.CheckedEntries)
	}

	// The full range verifies end to end.
	m2, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{
		ExportDir: filepath.Join(t.TempDir(), "full"), SigningKeys: keys,
	})
	if err != nil {
		t.Fatalf("ExportAuditChain full: %v", err)
	}
	if len(m2.Files) != 2 {
		t.Fatalf("full export Files = %d, want 2", len(m2.Files))
	}
	if !m2.Report.Ok {
		t.Errorf("full-range Report.Ok = false, want true (report: %+v)", m2.Report)
	}
	if m2.Report.CheckedEntries != 4 {
		t.Errorf("full-range CheckedEntries = %d, want 4", m2.Report.CheckedEntries)
	}
	// Files are oldest-first.
	if m2.Files[0].Day != "2026-10-09" || m2.Files[1].Day != "2026-10-10" {
		t.Errorf("Files order = %q,%q, want 2026-10-09 then 2026-10-10", m2.Files[0].Day, m2.Files[1].Day)
	}
}

func TestExportAuditChainTamperDetected(t *testing.T) {
	al := mustChainAuditLog(t)
	logTestEvents(t, al, 3)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	srcFile := auditDayFile(t, al.Dir())

	// Tamper with the source: rewrite line 2's amount in place while
	// keeping the stored seal, so the chain reports hash_mismatch.
	lines := readLines(t, srcFile)
	var m2 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &m2); err != nil {
		t.Fatalf("unmarshal line 2: %v", err)
	}
	m2["details"].(map[string]any)["amount_cents"] = float64(777)
	tampered, _ := json.Marshal(m2)
	lines[1] = string(tampered)
	writeLines(t, srcFile, lines)
	tamperedBytes, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("re-read tampered source: %v", err)
	}

	out := filepath.Join(t.TempDir(), "export")
	m, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: out, SigningKeys: chainTestSigningKeys(t)})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}
	// The export still completes; the report shows not-ok.
	if m.Report.Ok {
		t.Error("Report.Ok = true after tampering, want false")
	}
	if m.Report.CheckedEntries != 1 {
		t.Errorf("CheckedEntries = %d, want 1 (break on line 2)", m.Report.CheckedEntries)
	}
	// The snapshot is forensic: it captured the tampered bytes.
	if m.Files[0].SHA256 != sha256HexOfFile(t, srcFile) {
		t.Error("manifest SHA256 does not match the tampered source bytes")
	}
	if len(tamperedBytes) == 0 {
		t.Fatal("tamperedBytes empty")
	}
	rep, err := VerifyAuditLog(out)
	if err != nil {
		t.Fatalf("VerifyAuditLog(export): %v", err)
	}
	if rep.FirstBreak == nil || rep.FirstBreak.Reason != "hash_mismatch" {
		t.Errorf("FirstBreak = %+v, want hash_mismatch", rep.FirstBreak)
	}
	// The source file is byte-identical to the tampered input: the
	// export copied it, never rewrote it.
	after, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if string(after) != string(tamperedBytes) {
		t.Error("source file changed under the export")
	}
}

func TestExportAuditChainEmptyDir(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "export")
	m, err := ExportAuditChain(dir, AuditChainExportOptions{ExportDir: out})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}
	if len(m.Files) != 0 {
		t.Errorf("Files = %d, want 0", len(m.Files))
	}
	if !m.Report.Ok {
		t.Error("empty-dir Report.Ok = false, want true")
	}
	if m.Report.Head != AuditGenesisPrevHash {
		t.Errorf("empty-dir Head = %q, want the genesis marker", m.Report.Head)
	}
	if m.Report.FilesChecked != 0 {
		t.Errorf("empty-dir FilesChecked = %d, want 0", m.Report.FilesChecked)
	}
	if m.Signed || m.Signature != nil {
		t.Error("empty-dir export without keys must be unsigned")
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Errorf("manifest.json missing: %v", err)
	}
}

func TestExportAuditChainUnsigned(t *testing.T) {
	// Unsigned mode: no keys anywhere — the manifest must say so
	// honestly instead of failing. The fixture events use the wall-clock
	// day, because the audit_signing_disabled alert the unsigned log
	// records at construction also lands in today's file — pinning the
	// events to a fixed day would split the chain across day-files and
	// the replay would honestly report prev_mismatch (the midnight-UTC
	// straddle inside this test is a sub-second window and is ignored).
	al := mustAuditLog(t)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		ev := sealTestEvent("post", fmt.Sprintf("unsigned-trace-%d", i), uint64(i))
		ev.Timestamp = now.Add(time.Duration(i) * time.Second)
		if !al.Log(ev) {
			t.Fatalf("Log event %d: dropped", i)
		}
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: filepath.Join(t.TempDir(), "export")})
	if err != nil {
		t.Fatalf("ExportAuditChain: %v", err)
	}
	if m.Signed {
		t.Error("Signed = true without keys, want false")
	}
	if m.Signature != nil {
		t.Errorf("Signature = %+v without keys, want nil", m.Signature)
	}
	if !m.Report.Ok {
		t.Errorf("Report.Ok = false, want true (report: %+v)", m.Report)
	}
	if m.Report.CheckedEntries != 3 {
		// Two test events plus the audit_signing_disabled alert the
		// unsigned log records at construction.
		t.Errorf("CheckedEntries = %d, want 3 (2 events + signing-disabled alert)", m.Report.CheckedEntries)
	}
}

func TestExportAuditChainManifestDeterministic(t *testing.T) {
	al := mustChainAuditLog(t)
	logEventsOnDay(t, al, "2026-10-09", 2)
	logEventsOnDay(t, al, "2026-10-10", 1)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	keys := chainTestSigningKeys(t)
	normalize := func(t *testing.T, m AuditChainExportManifest) []byte {
		t.Helper()
		m.ExportedAt = "" // the only intentionally non-deterministic field
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}
	m1, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: filepath.Join(t.TempDir(), "a"), SigningKeys: keys})
	if err != nil {
		t.Fatalf("export 1: %v", err)
	}
	m2, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: filepath.Join(t.TempDir(), "b"), SigningKeys: keys})
	if err != nil {
		t.Fatalf("export 2: %v", err)
	}
	if got, want := normalize(t, m1), normalize(t, m2); string(got) != string(want) {
		t.Errorf("manifests differ on identical input:\n%s\n%s", got, want)
	}
	// Same for the unsigned path.
	u1, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: filepath.Join(t.TempDir(), "c")})
	if err != nil {
		t.Fatalf("export 3: %v", err)
	}
	u2, err := ExportAuditChain(al.Dir(), AuditChainExportOptions{ExportDir: filepath.Join(t.TempDir(), "d")})
	if err != nil {
		t.Fatalf("export 4: %v", err)
	}
	if got, want := normalize(t, u1), normalize(t, u2); string(got) != string(want) {
		t.Errorf("unsigned manifests differ on identical input:\n%s\n%s", got, want)
	}
}

func TestExportAuditChainBadInput(t *testing.T) {
	al := mustChainAuditLog(t)
	logTestEvents(t, al, 1)
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cases := []struct {
		name string
		opts AuditChainExportOptions
	}{
		{"bad from_day", AuditChainExportOptions{FromDay: "not-a-day", ExportDir: t.TempDir()}},
		{"bad to_day", AuditChainExportOptions{ToDay: "2026-13-40", ExportDir: t.TempDir()}},
		{"from after to", AuditChainExportOptions{FromDay: "2026-10-10", ToDay: "2026-10-09", ExportDir: t.TempDir()}},
		{"missing export dir", AuditChainExportOptions{}},
		{"export dir is audit dir", AuditChainExportOptions{ExportDir: al.Dir()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExportAuditChain(al.Dir(), tc.opts); !IsAuditExportInputError(err) {
				t.Errorf("err = %v, want an *AuditExportInputError", err)
			}
		})
	}
}

func TestSignAuditManifest(t *testing.T) {
	keys := chainTestSigningKeys(t)
	canon := []byte(`{"format":"audit-chain-export/1","exported_at":"2026-10-18T00:00:00Z"}`)
	sig, err := SignAuditManifest(canon, keys)
	if err != nil {
		t.Fatalf("SignAuditManifest: %v", err)
	}
	if sig.KeyID != "k1" {
		t.Errorf("KeyID = %q, want k1", sig.KeyID)
	}
	// Recompute by hand per the convention: HMAC-SHA256(key, kid||0x00||bytes).
	mac := hmac.New(sha256.New, keys[0].Key)
	mac.Write([]byte("k1"))
	mac.Write([]byte{0})
	mac.Write(canon)
	if want := hex.EncodeToString(mac.Sum(nil)); sig.MAC != want {
		t.Errorf("MAC = %s, want %s", sig.MAC, want)
	}
	// The MAC covers the bytes: flipping a byte must not verify.
	mac2 := hmac.New(sha256.New, keys[0].Key)
	mac2.Write([]byte("k1"))
	mac2.Write([]byte{0})
	mac2.Write([]byte(strings.Replace(string(canon), "2026", "2027", 1)))
	if hex.EncodeToString(mac2.Sum(nil)) == sig.MAC {
		t.Error("MAC verifies against altered bytes")
	}
	if _, err := SignAuditManifest(canon, nil); err == nil {
		t.Error("SignAuditManifest with no keys: want an error")
	}
}
