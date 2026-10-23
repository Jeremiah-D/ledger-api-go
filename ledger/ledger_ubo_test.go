package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Single-level declaration resolves to the declared persons with
// their exact shares, flagged controlling at the default 25%.
func TestSetUboResolveSingleLevel(t *testing.T) {
	l := New()
	if err := l.SetUbo("corp-a", []UboOwner{
		{Name: "Alice", Kind: UboOwnerPerson, ShareBP: 6000},
		{Name: "Bob", Kind: UboOwnerPerson, ShareBP: 4000},
	}); err != nil {
		t.Fatalf("SetUbo: %v", err)
	}
	decl, ok := l.GetUbo("corp-a")
	if !ok {
		t.Fatal("GetUbo(corp-a) = not found after SetUbo")
	}
	if len(decl.Owners) != 2 {
		t.Fatalf("GetUbo owners = %d, want 2", len(decl.Owners))
	}
	if decl.UpdatedAt.IsZero() {
		t.Fatal("GetUbo UpdatedAt is zero")
	}
	res, err := l.ResolveUbos("corp-a")
	if err != nil {
		t.Fatalf("ResolveUbos: %v", err)
	}
	if res.ThresholdBP != DefaultUboControlThresholdBP {
		t.Fatalf("ThresholdBP = %d, want %d", res.ThresholdBP, DefaultUboControlThresholdBP)
	}
	if len(res.Owners) != 2 {
		t.Fatalf("resolved owners = %d, want 2", len(res.Owners))
	}
	// Sorted by name: Alice first.
	if res.Owners[0].Name != "Alice" || res.Owners[0].EffectiveBP != 6000 || !res.Owners[0].Controlling {
		t.Fatalf("Alice = %+v, want 6000bp controlling", res.Owners[0])
	}
	if res.Owners[1].Name != "Bob" || res.Owners[1].EffectiveBP != 4000 || !res.Owners[1].Controlling {
		t.Fatalf("Bob = %+v, want 4000bp controlling", res.Owners[1])
	}
	if _, ok := l.GetUbo("unknown"); ok {
		t.Fatal("GetUbo(unknown) = found, want not found")
	}
	if res, err := l.ResolveUbos("unknown"); err != nil || len(res.Owners) != 0 {
		t.Fatalf("ResolveUbos(unknown) = %+v, %v; want empty, nil", res, err)
	}
}

// Multi-layer penetration: holding-H owns 100% of corp-b; H is owned
// 60/40 by Alice/Bob; Alice also owns 10% of corp-b directly.
// Alice: 10000*0.6 + 1000 = 7000bp; Bob: 4000bp.
func TestSetUboPenetrationMultiLayer(t *testing.T) {
	l := New()
	if err := l.SetUbo("holding-h", []UboOwner{
		{Name: "Alice", Kind: UboOwnerPerson, ShareBP: 6000},
		{Name: "Bob", Kind: UboOwnerPerson, ShareBP: 4000},
	}); err != nil {
		t.Fatalf("SetUbo holding-h: %v", err)
	}
	if err := l.SetUbo("corp-b", []UboOwner{
		{Name: "holding-h", Kind: UboOwnerEntity, ShareBP: 9000},
		{Name: "Alice", Kind: UboOwnerPerson, ShareBP: 1000},
	}); err != nil {
		t.Fatalf("SetUbo corp-b: %v", err)
	}
	res, err := l.ResolveUbos("corp-b")
	if err != nil {
		t.Fatalf("ResolveUbos: %v", err)
	}
	if len(res.Owners) != 2 {
		t.Fatalf("resolved owners = %+v, want Alice+Bob", res.Owners)
	}
	if res.Owners[0].Name != "Alice" || res.Owners[0].EffectiveBP != 6400 {
		t.Fatalf("Alice = %+v, want 6400bp", res.Owners[0])
	}
	if res.Owners[1].Name != "Bob" || res.Owners[1].EffectiveBP != 3600 {
		t.Fatalf("Bob = %+v, want 3600bp", res.Owners[1])
	}
}

// A cycle in the ownership graph fails the whole resolution.
func TestSetUboCycleRejected(t *testing.T) {
	l := New()
	if err := l.SetUbo("ent-a", []UboOwner{{Name: "ent-b", Kind: UboOwnerEntity, ShareBP: 10000}}); err != nil {
		t.Fatalf("SetUbo ent-a: %v", err)
	}
	if err := l.SetUbo("ent-b", []UboOwner{{Name: "ent-a", Kind: UboOwnerEntity, ShareBP: 10000}}); err != nil {
		t.Fatalf("SetUbo ent-b: %v", err)
	}
	if _, err := l.ResolveUbos("ent-a"); !errors.Is(err, ErrUboCycle) {
		t.Fatalf("ResolveUbos cycle = %v, want ErrUboCycle", err)
	}
	// Self-ownership is a cycle of length one.
	if err := l.SetUbo("ent-c", []UboOwner{{Name: "ent-c", Kind: UboOwnerEntity, ShareBP: 10000}}); err != nil {
		t.Fatalf("SetUbo ent-c: %v", err)
	}
	if _, err := l.ResolveUbos("ent-c"); !errors.Is(err, ErrUboCycle) {
		t.Fatalf("ResolveUbos self-cycle = %v, want ErrUboCycle", err)
	}
}

// An entity owner with no declaration is reported as unresolved —
// never silently dropped.
func TestSetUboUnresolvedEntity(t *testing.T) {
	l := New()
	if err := l.SetUbo("corp-d", []UboOwner{
		{Name: "ghost-holding", Kind: UboOwnerEntity, ShareBP: 7000},
		{Name: "Carol", Kind: UboOwnerPerson, ShareBP: 3000},
	}); err != nil {
		t.Fatalf("SetUbo: %v", err)
	}
	res, err := l.ResolveUbos("corp-d")
	if err != nil {
		t.Fatalf("ResolveUbos: %v", err)
	}
	if len(res.Owners) != 2 {
		t.Fatalf("resolved owners = %+v, want 2", res.Owners)
	}
	// Sorted: Carol, ghost-holding.
	if res.Owners[1].Name != "ghost-holding" || !res.Owners[1].Unresolved || res.Owners[1].EffectiveBP != 7000 {
		t.Fatalf("ghost-holding = %+v, want 7000bp unresolved", res.Owners[1])
	}
	if res.Owners[1].Controlling {
		t.Fatal("unresolved entity must never be flagged controlling")
	}
}

// Declarations whose shares exceed 100% — or carry malformed links —
// are refused.
func TestSetUboValidation(t *testing.T) {
	l := New()
	person := func(name string, bp int64) UboOwner { return UboOwner{Name: name, Kind: UboOwnerPerson, ShareBP: bp} }
	cases := []struct {
		name    string
		account AccountID
		owners  []UboOwner
		want    error
	}{
		{"empty account", "", []UboOwner{person("A", 10000)}, ErrUboEmptyAccount},
		{"empty name", "x", []UboOwner{person("", 10000)}, ErrUboInvalidOwner},
		{"bad kind", "x", []UboOwner{{Name: "A", Kind: "trust", ShareBP: 10000}}, ErrUboInvalidOwner},
		{"zero share", "x", []UboOwner{person("A", 0)}, ErrUboInvalidOwner},
		{"over 10000 share", "x", []UboOwner{person("A", 10001)}, ErrUboInvalidOwner},
		{"sum over 100%", "x", []UboOwner{person("A", 6000), person("B", 5000)}, ErrUboSharesOverflow},
		{"duplicate owner", "x", []UboOwner{person("A", 5000), person("A", 5000)}, ErrUboInvalidOwner},
	}
	for _, tc := range cases {
		if err := l.SetUbo(tc.account, tc.owners); !errors.Is(err, tc.want) {
			t.Errorf("%s: SetUbo = %v, want %v", tc.name, err, tc.want)
		}
	}
	// A refused declaration leaves no trace.
	if _, ok := l.GetUbo("x"); ok {
		t.Fatal("GetUbo(x) = found after refused declaration")
	}
}

// The control threshold is configurable: raising it un-flags smaller
// owners without touching the effective shares.
func TestSetUboControlThreshold(t *testing.T) {
	l := New()
	if got := l.UboControlThreshold(); got != DefaultUboControlThresholdBP {
		t.Fatalf("default threshold = %d, want %d", got, DefaultUboControlThresholdBP)
	}
	if err := l.SetUboControlThreshold(0); !errors.Is(err, ErrUboInvalidThreshold) {
		t.Fatalf("threshold 0 = %v, want ErrUboInvalidThreshold", err)
	}
	if err := l.SetUboControlThreshold(10001); !errors.Is(err, ErrUboInvalidThreshold) {
		t.Fatalf("threshold 10001 = %v, want ErrUboInvalidThreshold", err)
	}
	if err := l.SetUbo("corp-e", []UboOwner{
		{Name: "Dave", Kind: UboOwnerPerson, ShareBP: 3000},
		{Name: "Erin", Kind: UboOwnerPerson, ShareBP: 7000},
	}); err != nil {
		t.Fatalf("SetUbo: %v", err)
	}
	if err := l.SetUboControlThreshold(5000); err != nil {
		t.Fatalf("SetUboControlThreshold: %v", err)
	}
	res, err := l.ResolveUbos("corp-e")
	if err != nil {
		t.Fatalf("ResolveUbos: %v", err)
	}
	if res.ThresholdBP != 5000 {
		t.Fatalf("ThresholdBP = %d, want 5000", res.ThresholdBP)
	}
	for _, o := range res.Owners {
		want := o.Name == "Erin"
		if o.Controlling != want {
			t.Fatalf("%s controlling = %v, want %v", o.Name, o.Controlling, want)
		}
	}
}

// Setting a declaration and changing the threshold land in the audit
// chain (ops "ubo_set" / "ubo_threshold_set"), with the version
// unchanged across the structural change.
func TestSetUboEntersAuditChain(t *testing.T) {
	al := mustAuditLog(t)
	defer al.Close()
	l := New(WithAuditLog(al))

	vBefore := l.version
	if err := l.SetUbo("corp-f", []UboOwner{{Name: "Frank", Kind: UboOwnerPerson, ShareBP: 10000}}); err != nil {
		t.Fatalf("SetUbo: %v", err)
	}
	if err := l.SetUboControlThreshold(1000); err != nil {
		t.Fatalf("SetUboControlThreshold: %v", err)
	}
	if l.version != vBefore {
		t.Fatalf("UBO config bumped version: %d -> %d", vBefore, l.version)
	}

	now := time.Now()
	if err := al.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, corrupt, err := ReadAuditLog(al.Dir(), now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("ReadAuditLog: %v", err)
	}
	if corrupt != 0 {
		t.Fatalf("corrupt events = %d, want 0", corrupt)
	}
	var ops []string
	for _, ev := range events {
		ops = append(ops, ev.Op)
		if ev.VersionBefore != ev.VersionAfter {
			t.Fatalf("op %q moved the version", ev.Op)
		}
	}
	want := map[string]bool{"ubo_set": false, "ubo_threshold_set": false}
	for _, op := range ops {
		if _, ok := want[op]; ok {
			want[op] = true
		}
	}
	for op, seen := range want {
		if !seen {
			t.Fatalf("audit chain missing %q (ops: %v)", op, ops)
		}
	}
}

// Declarations and the threshold survive a snapshot round-trip.
func TestUboSnapshotRoundTrip(t *testing.T) {
	l := New()
	if err := l.SetUbo("corp-g", []UboOwner{
		{Name: "holding-x", Kind: UboOwnerEntity, ShareBP: 8000},
		{Name: "Grace", Kind: UboOwnerPerson, ShareBP: 2000},
	}); err != nil {
		t.Fatalf("SetUbo: %v", err)
	}
	if err := l.SetUboControlThreshold(3000); err != nil {
		t.Fatalf("SetUboControlThreshold: %v", err)
	}
	var buf bytes.Buffer
	if err := l.ExportSnapshot(&buf); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	rawSnapshot := append([]byte(nil), buf.Bytes()...)
	l2, err := ImportSnapshot(&buf)
	if err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	decl, ok := l2.GetUbo("corp-g")
	if !ok || len(decl.Owners) != 2 {
		t.Fatalf("restored declaration = %+v, %v; want 2 owners", decl, ok)
	}
	if got := l2.UboControlThreshold(); got != 3000 {
		t.Fatalf("restored threshold = %d, want 3000", got)
	}
	res, err := l2.ResolveUbos("corp-g")
	if err != nil {
		t.Fatalf("ResolveUbos after restore: %v", err)
	}
	if len(res.Owners) != 2 || res.Owners[0].Name != "Grace" || res.Owners[0].EffectiveBP != 2000 {
		t.Fatalf("resolved after restore = %+v", res.Owners)
	}
	// A snapshot written before UBO support (no UBO fields at all)
	// restores with no declarations and the default threshold: strip
	// the UBO fields from the config line of the JSON-lines snapshot.
	var stripped bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimRight(rawSnapshot, "\n"), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("unmarshal snapshot line: %v", err)
		}
		if rec["record"] == "config" {
			delete(rec, "ubo_declarations")
			delete(rec, "ubo_threshold_bp")
			line, err = json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal stripped config line: %v", err)
			}
		}
		stripped.Write(line)
		stripped.WriteByte('\n')
	}
	l3, err := ImportSnapshot(&stripped)
	if err != nil {
		t.Fatalf("ImportSnapshot stripped: %v", err)
	}
	if _, ok := l3.GetUbo("corp-g"); ok {
		t.Fatal("stripped snapshot restored a declaration")
	}
	if got := l3.UboControlThreshold(); got != DefaultUboControlThresholdBP {
		t.Fatalf("stripped snapshot threshold = %d, want default %d", got, DefaultUboControlThresholdBP)
	}
}
