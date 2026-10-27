package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func extendPartialFreezeSeed(t *testing.T, account AccountID, cents int64) *Ledger {
	t.Helper()
	l := New()
	seedBalance(t, l, account, 100000)
	if err := l.FreezeAmount(account, cents); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	return l
}

func TestExtendPartialFreezeExpirySuccess(t *testing.T) {
	l := extendPartialFreezeSeed(t, "alice", 30000)
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.SetPartialFreezeExpiry("alice", before); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	versionBefore := l.Version()
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := l.ExtendPartialFreezeExpiry("alice", extended); err != nil {
		t.Fatalf("ExtendPartialFreezeExpiry: %v", err)
	}
	// The extension moves the deadline and nothing else.
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("frozen = %d, want 30000", got)
	}
	if got, ok := l.PartialFreezeExpiry("alice"); !ok || !got.Equal(extended) {
		t.Errorf("expiry = %v, %v, want %v, true", got, ok, extended)
	}
	if l.Version() != versionBefore {
		t.Error("version moved: extension is off-journal")
	}
	// The FreezeAmount gate stays in place: the pair still
	// carries an expiry, so new freezes are still rejected
	// until the new deadline releases or is cleared.
	if err := l.FreezeAmount("alice", 1000); !errors.Is(err, ErrPartialFreezeExpirySet) {
		t.Errorf("freeze under extended expiry: err = %v, want ErrPartialFreezeExpirySet", err)
	}
	// The sweep reads the live deadline: a sweep at the old
	// deadline releases nothing, and one at the new deadline
	// releases the whole amount.
	if got := l.ExpirePartialFreezes(before); got != 0 {
		t.Errorf("sweep at old expiry released = %d, want 0", got)
	}
	if got := l.ExpirePartialFreezes(extended); got != 1 {
		t.Errorf("sweep at new expiry released = %d, want 1", got)
	}
	if got := l.PartialFrozenAmount("alice"); got != 0 {
		t.Errorf("frozen after new-expiry sweep = %d, want 0", got)
	}
}

func TestExtendPartialFreezeExpiryOnlyLengthens(t *testing.T) {
	l := extendPartialFreezeSeed(t, "alice", 30000)
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.SetPartialFreezeExpiry("alice", before); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	cases := map[string]time.Time{
		"equal":   before,
		"earlier": before.Add(-time.Minute),
		"past":    time.Now().Add(-time.Hour),
		"zero":    time.Time{},
	}
	for name, next := range cases {
		if err := l.ExtendPartialFreezeExpiry("alice", next); !errors.Is(err, ErrPartialFreezeExpiryExtensionNotLater) {
			t.Errorf("%s: err = %v, want ErrPartialFreezeExpiryExtensionNotLater", name, err)
		}
	}
	// Every rejected extension leaves the original deadline intact.
	if got, ok := l.PartialFreezeExpiry("alice"); !ok || !got.Equal(before) {
		t.Errorf("expiry after rejections = %v, %v, want %v, true", got, ok, before)
	}
	if got := l.PartialFrozenAmount("alice"); got != 30000 {
		t.Errorf("frozen after rejections = %d, want 30000", got)
	}
	// And a valid extension still works afterwards.
	if err := l.ExtendPartialFreezeExpiry("alice", time.Now().Add(2*time.Hour)); err != nil {
		t.Errorf("valid extension after rejections: %v", err)
	}
}

func TestExtendPartialFreezeExpiryRequiresSetExpiry(t *testing.T) {
	// No partial freeze at all.
	if err := New().ExtendPartialFreezeExpiry("ghost", time.Now().Add(time.Hour)); !errors.Is(err, ErrPartialFreezeNoFreeze) {
		t.Errorf("no freeze: err = %v, want ErrPartialFreezeNoFreeze", err)
	}
	// Freeze exists but no expiry was ever set: the operator
	// sets one first, an extension must not invent it.
	l := extendPartialFreezeSeed(t, "alice", 30000)
	if err := l.ExtendPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); !errors.Is(err, ErrPartialFreezeExpiryNotSet) {
		t.Errorf("no expiry: err = %v, want ErrPartialFreezeExpiryNotSet", err)
	}
	// A cleared expiry is likewise not extendable.
	if err := l.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	if err := l.ClearPartialFreezeExpiry("alice"); err != nil {
		t.Fatalf("ClearPartialFreezeExpiry: %v", err)
	}
	if err := l.ExtendPartialFreezeExpiry("alice", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrPartialFreezeExpiryNotSet) {
		t.Errorf("cleared expiry: err = %v, want ErrPartialFreezeExpiryNotSet", err)
	}
	// A fully released freeze has no expiry left either.
	l2 := extendPartialFreezeSeed(t, "bob", 30000)
	if err := l2.SetPartialFreezeExpiry("bob", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	if got := l2.ExpirePartialFreezes(time.Now().Add(2 * time.Hour)); got != 1 {
		t.Fatalf("sweep released = %d, want 1", got)
	}
	if err := l2.ExtendPartialFreezeExpiry("bob", time.Now().Add(3*time.Hour)); !errors.Is(err, ErrPartialFreezeNoFreeze) {
		t.Errorf("released freeze: err = %v, want ErrPartialFreezeNoFreeze", err)
	}
}

func TestExtendPartialFreezeExpiryPerCurrency(t *testing.T) {
	l := New()
	seedBalanceIn(t, l, "alice", 100000, "USD")
	seedBalanceIn(t, l, "alice", 100000, "EUR")
	if err := l.FreezeAmountIn("alice", "USD", 20000); err != nil {
		t.Fatalf("FreezeAmountIn USD: %v", err)
	}
	if err := l.FreezeAmountIn("alice", "EUR", 10000); err != nil {
		t.Fatalf("FreezeAmountIn EUR: %v", err)
	}
	usdExpiry := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.SetPartialFreezeExpiryIn("alice", "USD", usdExpiry); err != nil {
		t.Fatalf("SetPartialFreezeExpiryIn USD: %v", err)
	}
	// EUR has a freeze but no expiry: not extendable, and the
	// USD extension must not touch it.
	if err := l.ExtendPartialFreezeExpiryIn("alice", "EUR", time.Now().Add(2*time.Hour)); !errors.Is(err, ErrPartialFreezeExpiryNotSet) {
		t.Errorf("EUR no expiry: err = %v, want ErrPartialFreezeExpiryNotSet", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := l.ExtendPartialFreezeExpiryIn("alice", "USD", extended); err != nil {
		t.Fatalf("ExtendPartialFreezeExpiryIn USD: %v", err)
	}
	if got, ok := l.PartialFreezeExpiryIn("alice", "USD"); !ok || !got.Equal(extended) {
		t.Errorf("USD expiry = %v, %v, want %v, true", got, ok, extended)
	}
	if _, ok := l.PartialFreezeExpiryIn("alice", "EUR"); ok {
		t.Error("USD extension created a EUR expiry")
	}
	if got := l.PartialFrozenAmountIn("alice", "EUR"); got != 10000 {
		t.Errorf("EUR frozen = %d, want 10000", got)
	}
}

func TestExtendPartialFreezeExpirySnapshotRoundTrip(t *testing.T) {
	src := extendPartialFreezeSeed(t, "alice", 30000)
	if err := src.SetPartialFreezeExpiry("alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := src.ExtendPartialFreezeExpiry("alice", extended); err != nil {
		t.Fatalf("ExtendPartialFreezeExpiry: %v", err)
	}
	var buf bytes.Buffer
	if err := src.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	restored, err := ImportSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got, ok := restored.PartialFreezeExpiry("alice"); !ok || !got.Equal(extended) {
		t.Errorf("restored expiry = %v, %v, want %v, true", got, ok, extended)
	}
	// The restored extended deadline still gates the sweep.
	if got := restored.ExpirePartialFreezes(time.Now().Add(2 * time.Hour)); got != 0 {
		t.Errorf("restored sweep before new expiry released = %d, want 0", got)
	}
	if got := restored.ExpirePartialFreezes(extended); got != 1 {
		t.Errorf("restored sweep at new expiry released = %d, want 1", got)
	}
}

func TestExtendPartialFreezeExpiryAudit(t *testing.T) {
	dir := t.TempDir()
	al, err := NewAuditLog(dir)
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	l := New(WithAuditLog(al))
	seedBalance(t, l, "alice", 100000)
	if err := l.FreezeAmount("alice", 30000); err != nil {
		t.Fatalf("FreezeAmount: %v", err)
	}
	before := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := l.SetPartialFreezeExpiry("alice", before); err != nil {
		t.Fatalf("SetPartialFreezeExpiry: %v", err)
	}
	extended := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := l.ExtendPartialFreezeExpiry("alice", extended); err != nil {
		t.Fatalf("ExtendPartialFreezeExpiry: %v", err)
	}
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events := auditEventsByOp(t, dir)["partial_freeze_expiry_extended"]
	if len(events) != 1 {
		t.Fatalf("extend events = %d, want 1", len(events))
	}
	d := events[0].Details
	if d["currency"] != DefaultCurrency || d["frozen_cents"] != float64(30000) {
		t.Errorf("event details = %+v, want default currency, 30000 cents", d)
	}
	if d["previous_expires_at"] != before.UTC().Format(time.RFC3339) {
		t.Errorf("previous_expires_at = %v, want %v", d["previous_expires_at"], before.UTC().Format(time.RFC3339))
	}
	if d["expires_at"] != extended.UTC().Format(time.RFC3339) {
		t.Errorf("expires_at = %v, want %v", d["expires_at"], extended.UTC().Format(time.RFC3339))
	}
}
