package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestExtendFreezeUntilSuccess(t *testing.T) {
	l := New()
	before := time.Now().Add(200 * time.Millisecond)
	if err := l.FreezeUntil("alice", before); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	versionBefore := l.Version()
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := l.ExtendFreezeUntil("alice", extended); err != nil {
		t.Fatalf("ExtendFreezeUntil: %v", err)
	}
	// The extension moves the deadline and nothing else.
	if !l.IsFrozen("alice") {
		t.Error("account unfrozen by extension")
	}
	if got, ok := l.FreezeAutoUnfreezeAt("alice"); !ok || !got.Equal(extended) {
		t.Errorf("deadline = %v, %v, want %v, true", got, ok, extended)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: extension is off-journal")
	}
	// The sweep reads the live deadline: once the original
	// deadline passes, a release scan still releases nothing —
	// the extended freeze composes with the lazy/sweep release
	// instead of fighting it.
	time.Sleep(300 * time.Millisecond)
	if got := l.ExpireFrozenAccounts(); got != 0 {
		t.Errorf("release scan past old deadline released = %d, want 0", got)
	}
	if !l.IsFrozen("alice") {
		t.Error("account released at the old deadline despite extension")
	}
}

func TestExtendFreezeUntilOnlyLengthens(t *testing.T) {
	l := New()
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.FreezeUntil("alice", before); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	cases := map[string]time.Time{
		"equal":   before,
		"earlier": before.Add(-time.Minute),
		"past":    time.Now().Add(-time.Hour),
		"zero":    time.Time{},
	}
	for name, next := range cases {
		if err := l.ExtendFreezeUntil("alice", next); !errors.Is(err, ErrFreezeUntilExtensionNotLater) {
			t.Errorf("%s: err = %v, want ErrFreezeUntilExtensionNotLater", name, err)
		}
	}
	// Every rejected extension leaves the original deadline intact.
	if got, ok := l.FreezeAutoUnfreezeAt("alice"); !ok || !got.Equal(before) {
		t.Errorf("deadline after rejections = %v, %v, want %v, true", got, ok, before)
	}
	if !l.IsFrozen("alice") {
		t.Error("account unfrozen by rejected extensions")
	}
	// And a valid extension still works afterwards.
	if err := l.ExtendFreezeUntil("alice", time.Now().Add(2*time.Hour)); err != nil {
		t.Errorf("valid extension after rejections: %v", err)
	}
}

func TestExtendFreezeUntilRequiresTimedFreeze(t *testing.T) {
	// Never frozen.
	if err := New().ExtendFreezeUntil("ghost", time.Now().Add(time.Hour)); !errors.Is(err, ErrFreezeUntilNoTimedFreeze) {
		t.Errorf("unfrozen account: err = %v, want ErrFreezeUntilNoTimedFreeze", err)
	}
	// Manually frozen: no deadline to move.
	l := New()
	l.Freeze("bob")
	if err := l.ExtendFreezeUntil("bob", time.Now().Add(time.Hour)); !errors.Is(err, ErrFreezeUntilNoTimedFreeze) {
		t.Errorf("manual freeze: err = %v, want ErrFreezeUntilNoTimedFreeze", err)
	}
	if !l.IsFrozen("bob") {
		t.Error("rejected extension unfroze a manual freeze")
	}
	// Timed freeze re-frozen manually converts to a manual
	// freeze, so there is no deadline left to extend.
	l2 := New()
	if err := l2.FreezeUntil("carol", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	l2.Freeze("carol")
	if err := l2.ExtendFreezeUntil("carol", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrFreezeUntilNoTimedFreeze) {
		t.Errorf("manually re-frozen account: err = %v, want ErrFreezeUntilNoTimedFreeze", err)
	}
	// Already released: no deadline entry survives.
	l3 := New()
	if err := l3.FreezeUntil("dave", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	l3.Unfreeze("dave")
	if err := l3.ExtendFreezeUntil("dave", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrFreezeUntilNoTimedFreeze) {
		t.Errorf("unfrozen account: err = %v, want ErrFreezeUntilNoTimedFreeze", err)
	}
}

func TestExtendFreezeUntilSnapshotRoundTrip(t *testing.T) {
	src := New()
	if err := src.FreezeUntil("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := src.ExtendFreezeUntil("alice", extended); err != nil {
		t.Fatalf("ExtendFreezeUntil: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if !restored.IsFrozen("alice") {
		t.Error("restored account not frozen")
	}
	if got, ok := restored.FreezeAutoUnfreezeAt("alice"); !ok || !got.Equal(extended) {
		t.Errorf("restored deadline = %v, %v, want %v, true", got, ok, extended)
	}
	// The restored extended deadline still gates the release:
	// a sweep now releases nothing.
	if got := restored.ExpireFrozenAccounts(); got != 0 {
		t.Errorf("restored sweep released = %d, want 0", got)
	}
}

func TestExtendFreezeUntilAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.FreezeUntil("alice", before); err != nil {
		t.Fatalf("FreezeUntil: %v", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := l.ExtendFreezeUntil("alice", extended); err != nil {
		t.Fatalf("ExtendFreezeUntil: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["freeze_until_extended"]
	if len(events) != 1 {
		t.Fatalf("extend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["previous_auto_unfreeze_at"] != before.UTC().Format(time.RFC3339) {
		t.Errorf("previous_auto_unfreeze_at = %v, want %v", d["previous_auto_unfreeze_at"], before.UTC().Format(time.RFC3339))
	}
	if d["auto_unfreeze_at"] != extended.UTC().Format(time.RFC3339) {
		t.Errorf("auto_unfreeze_at = %v, want %v", d["auto_unfreeze_at"], extended.UTC().Format(time.RFC3339))
	}
}
