package ledger

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// amendTestKey builds a signing key from fixed (non-random) material so
// rotation tests are deterministic; these keys never leave the test
// process.
func amendTestKey(id, material string) AuditSigningKey {
	return AuditSigningKey{ID: id, Key: []byte(material)}
}

func amendTestKeys() (k1, k2, k3 AuditSigningKey) {
	k1 = amendTestKey("k1", "0123456789abcdef0123456789abcdef")
	k2 = amendTestKey("k2", "fedcba9876543210fedcba9876543210")
	k3 = amendTestKey("k3", "aaaabbbbccccddddeeeeffff00001111")
	return k1, k2, k3
}

// amendTestLedger builds a ledger with a signed audit log in a fresh
// temp dir.
func signAmendTestLedger(t *testing.T, keys []AuditSigningKey, grace time.Duration) (*Ledger, *AuditLog, string) {
	t.Helper()
	dir := t.TempDir()
	al, err := NewAuditLog(dir, WithAuditLogSigning(keys, grace))
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	return New(WithAuditLog(al)), al, dir
}

// amendTestPost posts one entry and fails the test on error.
func amendTestPost(t *testing.T, l *Ledger, id string) {
	t.Helper()
	if _, _, err := l.Post(JournalEntry{ID: id, DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
		t.Fatalf("Post %s: %v", id, err)
	}
}

// detailStringSlice extracts a []string details value (JSON round-trip
// through ReadAuditLog turns it into []any of string).
func detailStringSlice(t *testing.T, d map[string]any, key string) []string {
	t.Helper()
	v, ok := d[key]
	if !ok {
		t.Fatalf("details missing %q: %v", key, d)
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("details[%q] = %T, want []any", key, v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("details[%q] element = %T, want string", key, e)
		}
		out = append(out, s)
	}
	return out
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// amendAssertUntouched fails when a rejected rotation changed the live
// signing set or left a rotation event in the trail.
func amendAssertUntouched(t *testing.T, l *Ledger, al *AuditLog, dir string, wantIDs []string) {
	t.Helper()
	if got, ok := l.AuditSigningKeyIDs(); !ok || !equalStringSlice(got, wantIDs) {
		t.Fatalf("AuditSigningKeyIDs() = %v, %v; want %v, true (old set must be untouched)", got, ok, wantIDs)
	}
	if !al.Signed() {
		t.Fatal("Signed() = false after rejected rotation, want true (old set must be untouched)")
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if byOp := auditEventsByOp(t, dir); len(byOp["audit_signing_keys_rotated"]) != 0 {
		t.Fatal("rejected rotation wrote an audit_signing_keys_rotated event, want none")
	}
}

func TestRotateAuditSigningKeysEmptySetRejected(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	if err := l.RotateAuditSigningKeys(nil); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(nil) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{}); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys([]) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	amendAssertUntouched(t, l, al, dir, []string{"k1"})
}

func TestRotateAuditSigningKeysAllRetiredRejected(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	retired := time.Now().Add(-time.Hour).UTC()
	both := []AuditSigningKey{
		{ID: k2.ID, Key: k2.Key, RetiredAt: retired},
		{ID: k1.ID, Key: k1.Key, RetiredAt: retired},
	}
	if err := l.RotateAuditSigningKeys(both); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(all retired) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	amendAssertUntouched(t, l, al, dir, []string{"k1"})
}

func TestRotateAuditSigningKeysDuplicateIDRejected(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	dup := []AuditSigningKey{k2, {ID: "k2", Key: []byte("differentmaterial0123456789abcd")}}
	if err := l.RotateAuditSigningKeys(dup); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(duplicate id) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	amendAssertUntouched(t, l, al, dir, []string{"k1"})
}

func TestRotateAuditSigningKeysEmptyMaterialRejected(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k2, {ID: "k3"}}); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(empty material) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{{ID: "", Key: k2.Key}}); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(empty id) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{{ID: "   ", Key: k2.Key}}); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(blank id) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	amendAssertUntouched(t, l, al, dir, []string{"k1"})
}

func TestRotateAuditSigningKeysSameIDDifferentKeyRejected(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	impostor := []AuditSigningKey{{ID: "k1", Key: []byte("differentmaterial0123456789abcd")}}
	if err := l.RotateAuditSigningKeys(impostor); !errors.Is(err, ErrInvalidAuditSigningKeys) {
		t.Fatalf("RotateAuditSigningKeys(same id, different bytes) = %v, want ErrInvalidAuditSigningKeys", err)
	}
	amendAssertUntouched(t, l, al, dir, []string{"k1"})
}

func TestRotateAuditSigningKeysNoOp(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	retiredAt := time.Now().Add(-time.Hour).UTC()
	current := []AuditSigningKey{k1, {ID: k2.ID, Key: k2.Key, RetiredAt: retiredAt}}
	l, al, dir := signAmendTestLedger(t, current, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	// Rotating to the identical set item by item is a no-op.
	if err := l.RotateAuditSigningKeys(current); err != nil {
		t.Fatalf("RotateAuditSigningKeys(same set) = %v, want nil", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(dir, todayAuditDay())
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt = %d, want 0", corrupt)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (no-op rotation writes no audit event)", len(events))
	}
	if events[0].Op != "post" {
		t.Fatalf("event op = %q, want post", events[0].Op)
	}
}

func TestRotateAuditSigningKeysRotatesAndVerifies(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k2}); err != nil {
		t.Fatalf("RotateAuditSigningKeys([k2]) = %v", err)
	}
	// New rows carry the new primary kid; the old key is carried as
	// retired, primary first.
	if got, ok := l.AuditSigningKeyIDs(); !ok || !equalStringSlice(got, []string{"k2", "k1"}) {
		t.Fatalf("AuditSigningKeyIDs() = %v, %v; want [k2 k1], true", got, ok)
	}
	amendTestPost(t, l, "e2")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	rot := byOp["audit_signing_keys_rotated"]
	if len(rot) != 1 {
		t.Fatalf("audit_signing_keys_rotated events = %d, want 1", len(rot))
	}
	if !equalStringSlice(detailStringSlice(t, rot[0].Details, "previous_key_ids"), []string{"k1"}) {
		t.Fatalf("previous_key_ids = %v, want [k1]", rot[0].Details["previous_key_ids"])
	}
	if !equalStringSlice(detailStringSlice(t, rot[0].Details, "new_key_ids"), []string{"k2"}) {
		t.Fatalf("new_key_ids = %v, want [k2]", rot[0].Details["new_key_ids"])
	}
	if !equalStringSlice(detailStringSlice(t, rot[0].Details, "retired_key_ids"), []string{"k1"}) {
		t.Fatalf("retired_key_ids = %v, want [k1]", rot[0].Details["retired_key_ids"])
	}
	// The rotation event itself is signed by the new primary.
	if rot[0].Sig == nil || rot[0].Sig.KeyID != "k2" {
		t.Fatalf("rotation event sig = %+v, want kid k2", rot[0].Sig)
	}
	// e1 keeps kid k1, e2 carries kid k2.
	posts := byOp["post"]
	if len(posts) != 2 {
		t.Fatalf("post events = %d, want 2", len(posts))
	}
	for _, p := range posts {
		if p.Sig == nil {
			t.Fatalf("post %s has no sig", p.TraceID)
		}
	}
	if posts[0].Sig.KeyID != "k1" || posts[1].Sig.KeyID != "k2" {
		t.Fatalf("post kids = %q, %q; want k1, k2", posts[0].Sig.KeyID, posts[1].Sig.KeyID)
	}
	// Full set (new + retired, retiredAt from the rotation event's own
	// timestamp — e1 predates it) verifies all history rows.
	full := []AuditSigningKey{k2, {ID: k1.ID, Key: k1.Key, RetiredAt: rot[0].Timestamp}}
	rep, err := VerifyAuditSignatures(dir, full, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures(full set): %v", err)
	}
	if !rep.Ok {
		t.Fatalf("full-set verify not ok: %+v", rep.BadLines)
	}
	if rep.SignedLines != 3 {
		t.Fatalf("signed = %d, want 3 (e1 + rotation + e2)", rep.SignedLines)
	}
	// Passing only the new keys makes the old row report unknown_key —
	// the operator must assemble the full set from the event's groups.
	rep2, err := VerifyAuditSignatures(dir, []AuditSigningKey{k2}, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures(new keys only): %v", err)
	}
	if rep2.Ok {
		t.Fatal("new-keys-only verify ok, want unknown_key on the old row")
	}
	found := false
	for _, b := range rep2.BadLines {
		if b.Reason == "unknown_key" && b.TraceID == "e1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want unknown_key on e1, got %+v", rep2.BadLines)
	}
}

func TestRotateAuditSigningKeysConcurrentPostNoRace(t *testing.T) {
	k1, k2, k3 := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	sets := [][]AuditSigningKey{{k2}, {k3}, {k1}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := l.Post(JournalEntry{ID: fmt.Sprintf("e-%d-%d", n, j), DebitAccount: "cash", CreditAccount: "rev", AmountCents: 100}); err != nil {
					t.Errorf("Post: %v", err)
					return
				}
				_ = al.Signed()
				if _, ok := l.AuditSigningKeyIDs(); !ok {
					t.Errorf("AuditSigningKeyIDs() not enabled")
					return
				}
			}
		}(n)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 60; i++ {
			if err := l.RotateAuditSigningKeys(sets[i%len(sets)]); err != nil {
				t.Errorf("RotateAuditSigningKeys: %v", err)
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Every line verifies against the all-active full set: the
	// rotation history never changes the key bytes, only which key is
	// primary, so the MACs all recompute.
	rep, err := VerifyAuditSignatures(dir, []AuditSigningKey{k1, k2, k3}, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("concurrent verify not ok: %+v", rep.BadLines)
	}
	if rep.SignedLines == 0 {
		t.Fatal("signed = 0, want rows signed during the concurrent rotation")
	}
}

func TestRotateAuditSigningKeysUnsignedLedgerInstallsKeys(t *testing.T) {
	k1, _, _ := amendTestKeys()
	l, al, dir := retuneTestLedger(t) // no signing keys: degraded unsigned mode
	if al.Signed() {
		t.Fatal("Signed() = true without keys, want false")
	}
	amendTestPost(t, l, "e1")
	// Wait for both the construction-time signing-disabled alert and
	// e1 to be flushed unsigned: signing happens at flush time, so a
	// row still queued at rotation time would legitimately be signed
	// by the new primary (future-only is about flushed rows).
	waitAuditWritten(t, al, 2)
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k1}); err != nil {
		t.Fatalf("RotateAuditSigningKeys on unsigned ledger = %v", err)
	}
	if !al.Signed() {
		t.Fatal("Signed() = false after rotation, want true")
	}
	if got, ok := l.AuditSigningKeyIDs(); !ok || !equalStringSlice(got, []string{"k1"}) {
		t.Fatalf("AuditSigningKeyIDs() = %v, %v; want [k1], true", got, ok)
	}
	amendTestPost(t, l, "e2")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Old unsigned rows are counted, not failed; new rows are signed.
	rep, err := VerifyAuditSignatures(dir, []AuditSigningKey{k1}, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures: %v", err)
	}
	if !rep.Ok {
		t.Fatalf("verify not ok: %+v", rep.BadLines)
	}
	if rep.UnsignedLines != 2 {
		t.Fatalf("unsigned = %d, want 2 (signing-disabled alert + e1)", rep.UnsignedLines)
	}
	if rep.SignedLines != 2 {
		t.Fatalf("signed = %d, want 2 (rotation event + e2)", rep.SignedLines)
	}
	byOp := auditEventsByOp(t, dir)
	if len(byOp["audit_signing_keys_rotated"]) != 1 {
		t.Fatalf("rotation events = %d, want 1", len(byOp["audit_signing_keys_rotated"]))
	}
}

func TestRotateAuditSigningKeysCtxActor(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	ctx := ContextWithActor(context.Background(), "ops-alan")
	if err := l.RotateAuditSigningKeysCtx(ctx, []AuditSigningKey{k2}); err != nil {
		t.Fatalf("RotateAuditSigningKeysCtx = %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	byOp := auditEventsByOp(t, dir)
	rot := byOp["audit_signing_keys_rotated"]
	if len(rot) != 1 {
		t.Fatalf("rotation events = %d, want 1", len(rot))
	}
	if rot[0].Actor != "ops-alan" {
		t.Fatalf("rotation event actor = %q, want ops-alan", rot[0].Actor)
	}
}

func TestRotateAuditSigningKeysNoKeyMaterialInAudit(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k2}); err != nil {
		t.Fatalf("RotateAuditSigningKeys = %v", err)
	}
	amendTestPost(t, l, "e2")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// No key material anywhere in the audit directory: neither the raw
	// key bytes nor their hex encoding may appear in any file.
	needles := []string{
		string(k1.Key), hex.EncodeToString(k1.Key),
		string(k2.Key), hex.EncodeToString(k2.Key),
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, n := range needles {
			if strings.Contains(string(raw), n) {
				t.Fatalf("audit file %s contains key material %q", e.Name(), n)
			}
		}
	}
	// The rotation event's details carry ids only.
	byOp := auditEventsByOp(t, dir)
	rot := byOp["audit_signing_keys_rotated"]
	if len(rot) != 1 {
		t.Fatalf("rotation events = %d, want 1", len(rot))
	}
	for key, v := range rot[0].Details {
		if strings.Contains(key, "key") && !strings.HasSuffix(key, "_ids") {
			t.Fatalf("details key %q is not an id-group key", key)
		}
		_ = v
	}
	if got := detailStringSlice(t, rot[0].Details, "new_key_ids"); !equalStringSlice(got, []string{"k2"}) {
		t.Fatalf("new_key_ids = %v, want [k2]", got)
	}
}

func TestRotateAuditSigningKeysRetiredGrace(t *testing.T) {
	k1, k2, _ := amendTestKeys()
	l, al, dir := signAmendTestLedger(t, []AuditSigningKey{k1}, 24*time.Hour)
	amendTestPost(t, l, "e1")
	waitAuditWritten(t, al, 1)
	// Rotate k1 out with an explicit retirement 2h ago (passed
	// RetiredAt is kept): e1 was written after that timestamp, so it
	// is a retired-key row whose fate depends on the grace window.
	retiredAt := time.Now().Add(-2 * time.Hour).UTC()
	k1retired := AuditSigningKey{ID: k1.ID, Key: k1.Key, RetiredAt: retiredAt}
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k2, k1retired}); err != nil {
		t.Fatalf("RotateAuditSigningKeys = %v", err)
	}
	// The passed retired state is kept, not overwritten with the
	// rotation time.
	if got, ok := l.AuditSigningKeyIDs(); !ok || !equalStringSlice(got, []string{"k2", "k1"}) {
		t.Fatalf("AuditSigningKeyIDs() = %v, %v; want [k2 k1], true", got, ok)
	}
	amendTestPost(t, l, "e2")
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	full := []AuditSigningKey{k2, k1retired}
	rep, err := VerifyAuditSignatures(dir, full, 24*time.Hour)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures(24h grace): %v", err)
	}
	if !rep.Ok {
		t.Fatalf("24h grace verify not ok: %+v", rep.BadLines)
	}
	// With zero grace the e1 row (written after k1's retirement)
	// reports key_retired.
	rep0, err := VerifyAuditSignatures(dir, full, 0)
	if err != nil {
		t.Fatalf("VerifyAuditSignatures(0 grace): %v", err)
	}
	if rep0.Ok {
		t.Fatal("0 grace verify ok, want key_retired on e1")
	}
	found := false
	for _, b := range rep0.BadLines {
		if b.Reason == "key_retired" && b.TraceID == "e1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want key_retired on e1, got %+v", rep0.BadLines)
	}
}

func TestRotateAuditSigningKeysNoAuditLog(t *testing.T) {
	l := New() // no WithAuditLog
	k1, _, _ := amendTestKeys()
	if err := l.RotateAuditSigningKeys([]AuditSigningKey{k1}); !errors.Is(err, ErrAuditLogNotConfigured) {
		t.Fatalf("RotateAuditSigningKeys without audit log = %v, want ErrAuditLogNotConfigured", err)
	}
	if ids, ok := l.AuditSigningKeyIDs(); ok || ids != nil {
		t.Fatalf("AuditSigningKeyIDs() = %v, %v; want nil, false", ids, ok)
	}
}
