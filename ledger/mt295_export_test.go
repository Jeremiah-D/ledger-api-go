package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// mt295ExportTestLedger mirrors mt210TestLedger but wires an audit
// log, so the export marker event is assertable: one daily
// schedule, 10000 cents USD, next run 2026-11-12 09:00 UTC, BICs
// registered on both legs (alice DEUTDEFF, bob NWBKGB2L). Callers
// pause the schedule before exporting: an MT295 asks about the
// MT292 of a run whose notification premise is dead.
func mt295ExportTestLedger(t *testing.T, al *AuditLog) (*Ledger, TransferSchedule) {
	t.Helper()
	l := New(WithAuditLog(al))
	if err := l.SetAccountIBAN("alice", "DE89370400440532013000", "DEUTDEFF"); err != nil {
		t.Fatalf("SetAccountIBAN alice: %v", err)
	}
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	nextRun := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	sched, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID:          "sched-1",
		From:        "alice",
		To:          "bob",
		AmountCents: 10000,
		Currency:    "USD",
		FeeCents:    250,
		FeeAccount:  "fee-revenue",
		Interval:    24 * time.Hour,
		NextRunAt:   nextRun,
	})
	if err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	return l, sched
}

// mt295ExportOptions is the valid question for the fixture
// schedule: one coded :75: line, no narrative, no description.
func mt295ExportOptions(t *testing.T) MT295ExportOptions {
	t.Helper()
	return MT295ExportOptions{
		SenderLT:   "DEUTDEFF",
		ReceiverLT: "CHASUS33",
		Queries:    []string{"STAT"},
	}
}

// mt295ExportPausedLedger is the exportable fixture: the schedule
// paused, so its MT292 could have been sent and may be asked
// about.
func mt295ExportPausedLedger(t *testing.T, al *AuditLog) (*Ledger, TransferSchedule) {
	t.Helper()
	l, sched := mt295ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	return l, sched
}

func TestExportMT295RenderRoundTrip(t *testing.T) {
	m := MT295{
		SenderLT:             "DEUTDEFF",
		ReceiverLT:           "CHASUS33",
		TransactionReference: "MT295-abcdef1234",
		RelatedReference:     "MT292-0123456789",
		Queries:              []string{"STAT", "DUPL"},
		Narrative:            []string{"OPERATOR NOTE"},
		Description:          []string{"PLEASE CONFIRM", "WHETHER CANCELLED"},
	}
	doc, err := ExportMT295(m)
	if err != nil {
		t.Fatalf("ExportMT295: %v", err)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		"{3:{108:MT295-abcdef1234}}",
		"{4:",
		":20:MT295-abcdef1234",
		":21:MT292-0123456789",
		":75:STAT",
		"DUPL",
		":77A:OPERATOR NOTE",
		":79:PLEASE CONFIRM",
		"WHETHER CANCELLED",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("rendered MT295 missing %q, body:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "{5:") {
		t.Errorf("rendered MT295 must omit block 5, body:\n%s", doc)
	}
	parsed, err := ParseMT295(doc)
	if err != nil {
		t.Fatalf("ParseMT295(rendered): %v", err)
	}
	if parsed.SenderLT != "DEUTDEFFXXX" || parsed.ReceiverLT != "CHASUS33XXX" {
		t.Errorf("round-trip LTs = %q/%q, want DEUTDEFFXXX/CHASUS33XXX", parsed.SenderLT, parsed.ReceiverLT)
	}
	if parsed.TransactionReference != m.TransactionReference || parsed.RelatedReference != m.RelatedReference {
		t.Errorf("round-trip refs = %q/%q, want %q/%q", parsed.TransactionReference, parsed.RelatedReference, m.TransactionReference, m.RelatedReference)
	}
	if strings.Join(parsed.Queries, "\n") != strings.Join(m.Queries, "\n") ||
		strings.Join(parsed.Narrative, "\n") != strings.Join(m.Narrative, "\n") ||
		strings.Join(parsed.Description, "\n") != strings.Join(m.Description, "\n") {
		t.Errorf("round-trip texts = %q/%q/%q, want %q/%q/%q", parsed.Queries, parsed.Narrative, parsed.Description, m.Queries, m.Narrative, m.Description)
	}
}

func TestMT295MessageReferenceDeterministic(t *testing.T) {
	related := "MT292-0123456789"
	queries := []string{"STAT"}
	first := mt295MessageReference(related, queries, nil, nil)
	if len(first) != 16 || !strings.HasPrefix(first, "MT295-") {
		t.Fatalf(":20: = %q, want 16 characters starting MT295-", first)
	}
	if hexPart := first[len("MT295-"):]; hexPart != strings.ToLower(hexPart) {
		t.Fatalf(":20: hex part = %q, want lowercase hex", hexPart)
	}
	// Same question about the same cancellation request: the
	// same :20:.
	if again := mt295MessageReference(related, []string{"STAT"}, nil, nil); again != first {
		t.Errorf("same terms :20: = %q, want %q", again, first)
	}
	// A different question: a different :20:.
	if other := mt295MessageReference(related, []string{"DUPL"}, nil, nil); other == first {
		t.Errorf("different question :20: = %q, want a different reference from %q", other, first)
	}
	// A different queried request: a different :20:.
	if other := mt295MessageReference("MT292-abcdef1234", queries, nil, nil); other == first {
		t.Errorf("different :21: :20: = %q, want a different reference from %q", other, first)
	}
	// Narrative and description are part of the question body.
	if other := mt295MessageReference(related, queries, []string{"NOTE"}, nil); other == first {
		t.Errorf("narrative :20: = %q, want a different reference from %q", other, first)
	}
	if other := mt295MessageReference(related, queries, nil, []string{"DESC"}); other == first {
		t.Errorf("description :20: = %q, want a different reference from %q", other, first)
	}
}

func TestExportMT295ForCancelledRenders(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	before := l.Version()
	opts := mt295ExportOptions(t)
	res, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForCancelled: %v", err)
	}
	related := mt292ExpectedRef(sched.ID, sched.RunSeq)
	if res.RelatedReference != related {
		t.Errorf(":21: = %q, want the MT292 :20: %q", res.RelatedReference, related)
	}
	wantRef := mt295MessageReference(related, opts.Queries, opts.Narrative, opts.Description)
	if res.Reference != wantRef {
		t.Errorf(":20: = %q, want %q", res.Reference, wantRef)
	}
	if res.ScheduleID != sched.ID || res.RunSeq != sched.RunSeq {
		t.Errorf("schedule/run = %q/%d, want %q/%d", res.ScheduleID, res.RunSeq, sched.ID, sched.RunSeq)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		"{3:{108:" + wantRef + "}}",
		":20:" + wantRef,
		":21:" + related,
		":75:STAT",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q, body:\n%s", want, res.Message)
		}
	}
	if strings.Contains(res.Message, ":77A:") || strings.Contains(res.Message, ":79:") {
		t.Errorf("message must omit empty :77A:/:79:, body:\n%s", res.Message)
	}
	// Rendering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The schedule itself did not move: still paused.
	after, err := l.GetTransferSchedule(sched.ID)
	if err != nil {
		t.Fatalf("GetTransferSchedule: %v", err)
	}
	if after.Status != SchedulePaused {
		t.Errorf("schedule status = %q, want paused (rendering moves nothing)", after.Status)
	}
	// The stored export round-trips through the registry.
	stored, ok := l.GetMT295Export(wantRef)
	if !ok {
		t.Fatal("GetMT295Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.RelatedReference != related || stored.ScheduleID != sched.ID {
		t.Errorf("stored export = %+v, want the rendered message", stored)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, no accounts.
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 1 {
		t.Errorf("mt295_query_exported count = %d, want 1", countOp(events, "mt295_query_exported"))
	}
	ev := findOp(events, "mt295_query_exported")
	if ev == nil {
		t.Fatal("mt295_query_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT295ForCancelled)", ev.Actor)
	}
	if ev.TraceID != wantRef {
		t.Errorf("trace = %q, want %q", ev.TraceID, wantRef)
	}
	if ev.Details["msg_id"] != wantRef || ev.Details["related_reference"] != related ||
		ev.Details["schedule_id"] != sched.ID || ev.Details["sender_lt"] != "DEUTDEFFXXX" {
		t.Errorf("details = %v, want msg/related/schedule/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (a query touches no account)", ev.Accounts)
	}
}

func TestExportMT295ForCancelledUnknownSchedule(t *testing.T) {
	al := mustAuditLog(t)
	l, _ := mt295ExportTestLedger(t, al)
	opts := mt295ExportOptions(t)
	res, err := l.ExportMT295ForCancelled("no-such-schedule", 1, opts, "ledger-operator")
	if !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("err = %v, want ErrScheduleNotFound", err)
	}
	if res.RepairID == "" {
		t.Error("repair ID empty, want the parked export recorded")
	}
	// A parked export records nothing: no registry entry, no
	// audit marker.
	related := mt292MessageReference("no-such-schedule", 1)
	ref := mt295MessageReference(related, opts.Queries, opts.Narrative, opts.Description)
	if _, ok := l.GetMT295Export(ref); ok {
		t.Error("GetMT295Export = true for a parked export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 0 {
		t.Errorf("mt295_query_exported count = %d, want 0 for a parked export", countOp(events, "mt295_query_exported"))
	}
	// A re-sent export parks again.
	if _, err := l.ExportMT295ForCancelled("no-such-schedule", 1, opts, "ledger-operator"); !errors.Is(err, ErrScheduleNotFound) {
		t.Errorf("re-sent err = %v, want ErrScheduleNotFound again", err)
	}
}

func TestExportMT295ForCancelledStillActive(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportTestLedger(t, al)
	// Not paused: the schedule is active with a live next run,
	// so its MT292 cannot have been sent — the gate itself is
	// the proof, and the export parks fail-closed.
	res, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, mt295ExportOptions(t), "ledger-operator")
	if !errors.Is(err, ErrMT292ScheduleStillActive) {
		t.Fatalf("err = %v, want ErrMT292ScheduleStillActive", err)
	}
	if res.RepairID == "" {
		t.Error("repair ID empty, want the parked export recorded")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 0 {
		t.Errorf("mt295_query_exported count = %d, want 0 for a parked export", countOp(events, "mt295_query_exported"))
	}
}

func TestExportMT295ForCancelledRunSeqChangesReference(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	first, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForCancelled run %d: %v", sched.RunSeq, err)
	}
	second, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq+1, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForCancelled run %d: %v", sched.RunSeq+1, err)
	}
	if second.RelatedReference == first.RelatedReference {
		t.Errorf("run %d :21: = %q, want a different MT292 :20: from run %d", sched.RunSeq+1, second.RelatedReference, sched.RunSeq)
	}
	if second.Reference == first.Reference {
		t.Errorf("run %d :20: = %q, want a different reference from run %d", sched.RunSeq+1, second.Reference, sched.RunSeq)
	}
	if second.Message == first.Message {
		t.Error("different runs rendered the same message, want different :20:/:21:")
	}
}

func TestExportMT295ForCancelledRepeatIsDuplicate(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	first, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForCancelled: %v", err)
	}
	second, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForCancelled repeat: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on repeat export")
	}
	if second.Message != first.Message {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
	// The replay emits no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 1 {
		t.Errorf("mt295_query_exported count = %d, want 1 after replay", countOp(events, "mt295_query_exported"))
	}
}

func TestExportMT295ForCancelledConflict(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	related := mt292ExpectedRef(sched.ID, sched.RunSeq)
	ref := mt295MessageReference(related, opts.Queries, opts.Narrative, opts.Description)
	// A :20: already exported under different terms: the
	// reference names one export, so a second, different question
	// under it is refused rather than silently overwritten. The
	// derivation makes this unreachable through the public API
	// (the same terms hash to the same :20:), so the conflicting
	// record is seeded directly to exercise the gate itself.
	l.mt295Exports = map[string]mt295ExportRecord{
		ref: {
			QueriedRef:  related,
			QueriedKind: MT295QueriedMT292,
			Options: MT295ExportOptions{
				SenderLT:   "DEUTDEFF",
				ReceiverLT: "CHASUS33",
				Queries:    []string{"STAT"},
				Narrative:  []string{"A different question"},
			},
			Result: MT295ExportResult{Reference: ref, RelatedReference: related, ScheduleID: sched.ID, RunSeq: sched.RunSeq, Message: "seeded"},
		},
	}
	if _, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator"); !errors.Is(err, ErrMT295MsgIDConflict) {
		t.Errorf("err = %v, want ErrMT295MsgIDConflict", err)
	}
}

func TestExportMT295ForCancelledEmptyQueries(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	opts.Queries = nil
	if _, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator"); !errors.Is(err, ErrMT295Queries) {
		t.Errorf("err = %v, want ErrMT295Queries", err)
	}
}

func TestExportMT295ForCancelledQueriesTooManyLines(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	opts.Queries = []string{"L1", "L2", "L3", "L4", "L5", "L6", "L7"}
	if _, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator"); !errors.Is(err, ErrMT295Queries) {
		t.Errorf("err = %v, want ErrMT295Queries", err)
	}
}

func TestExportMT295ForCancelledQueryLineTooLong(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	opts.Queries = []string{strings.Repeat("x", 36)}
	if _, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator"); !errors.Is(err, ErrMT295Queries) {
		t.Errorf("err = %v, want ErrMT295Queries", err)
	}
}

func TestExportMT295ForCancelledNarrativeAndDescriptionVerbatim(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	opts.Queries = []string{"STAT", "DUPL"}
	opts.Narrative = []string{"OPERATOR NOTE", "SECOND LINE"}
	opts.Description = []string{"PLEASE CONFIRM", "WHETHER CANCELLED"}
	res, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForCancelled: %v", err)
	}
	// The :77A: and :79: ride verbatim, in canonical order, after
	// the :75: — the operator's words, never re-derived.
	for _, want := range []string{
		":75:STAT\nDUPL",
		":77A:OPERATOR NOTE\nSECOND LINE",
		":79:PLEASE CONFIRM\nWHETHER CANCELLED",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q, body:\n%s", want, res.Message)
		}
	}
	if idx75, idx77, idx79 := strings.Index(res.Message, ":75:"), strings.Index(res.Message, ":77A:"), strings.Index(res.Message, ":79:"); !(idx75 < idx77 && idx77 < idx79) {
		t.Errorf("field order wrong: :75: at %d, :77A: at %d, :79: at %d", idx75, idx77, idx79)
	}
	// A replay returns the stored verbatim question, and the
	// getter returns it too.
	stored, ok := l.GetMT295Export(res.Reference)
	if !ok {
		t.Fatal("GetMT295Export = false, want the stored export")
	}
	if stored.Message != res.Message {
		t.Errorf("stored message differs:\n%v\n%v", stored.Message, res.Message)
	}
	parsed, err := ParseMT295(res.Message)
	if err != nil {
		t.Fatalf("ParseMT295(rendered): %v", err)
	}
	if strings.Join(parsed.Narrative, "\n") != "OPERATOR NOTE\nSECOND LINE" ||
		strings.Join(parsed.Description, "\n") != "PLEASE CONFIRM\nWHETHER CANCELLED" {
		t.Errorf("round-trip narrative/description = %q/%q, want the verbatim lines", parsed.Narrative, parsed.Description)
	}
}

func TestExportMT295ForCancelledBadSenderLT(t *testing.T) {
	al := mustAuditLog(t)
	l, sched := mt295ExportPausedLedger(t, al)
	opts := mt295ExportOptions(t)
	opts.SenderLT = "NOT-A-BIC"
	if _, err := l.ExportMT295ForCancelled(sched.ID, sched.RunSeq, opts, "ledger-operator"); !errors.Is(err, ErrMT295LT) {
		t.Errorf("err = %v, want ErrMT295LT", err)
	}
}

func TestExportMT295ForMT202Renders(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	before := l.Version()
	// Register the outbound MT202 first: the query anchors
	// against the registry, never against a shape.
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	opts := mt295ExportOptions(t)
	res, err := l.ExportMT295ForMT202("MT202-REF-1", opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForMT202: %v", err)
	}
	// The :21: is the MT202's own :20:, caller-supplied —
	// no derivation, no shape.
	if res.RelatedReference != "MT202-REF-1" {
		t.Errorf(":21: = %q, want the MT202 :20: %q", res.RelatedReference, "MT202-REF-1")
	}
	wantRef := mt295MessageReference("MT202-REF-1", opts.Queries, opts.Narrative, opts.Description)
	if res.Reference != wantRef {
		t.Errorf(":20: = %q, want %q", res.Reference, wantRef)
	}
	// No schedule or run on this tier — the kind is what
	// tells it apart from the MT292 tier.
	if res.ScheduleID != "" || res.RunSeq != 0 {
		t.Errorf("schedule/run = %q/%d, want empty/0 on the MT202 tier", res.ScheduleID, res.RunSeq)
	}
	if res.QueriedKind != MT295QueriedMT202 {
		t.Errorf("queried kind = %q, want %q", res.QueriedKind, MT295QueriedMT202)
	}
	for _, want := range []string{
		"{1:F01DEUTDEFFXXX0000000000}",
		"{2:I295CHASUS33XXXU3003}",
		"{3:{108:" + wantRef + "}}",
		":20:" + wantRef,
		":21:MT202-REF-1",
		":75:STAT",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q, body:\n%s", want, res.Message)
		}
	}
	// Rendering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The stored export round-trips through the registry,
	// kind and all.
	stored, ok := l.GetMT295Export(wantRef)
	if !ok {
		t.Fatal("GetMT295Export = false, want the stored export")
	}
	if stored.Message != res.Message || stored.RelatedReference != "MT202-REF-1" || stored.QueriedKind != MT295QueriedMT202 {
		t.Errorf("stored export = %+v, want the rendered MT202-tier message", stored)
	}
	// Audit: exactly one export marker, with the exact details —
	// the :20: on the durable record, the kind instead of a
	// schedule, no accounts.
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 1 {
		t.Errorf("mt295_query_exported count = %d, want 1", countOp(events, "mt295_query_exported"))
	}
	ev := findOp(events, "mt295_query_exported")
	if ev == nil {
		t.Fatal("mt295_query_exported event missing")
	}
	if ev.Actor != "ledger-operator" {
		t.Errorf("actor = %q, want ledger-operator (ExportMT295ForMT202)", ev.Actor)
	}
	if ev.TraceID != wantRef {
		t.Errorf("trace = %q, want %q", ev.TraceID, wantRef)
	}
	if ev.Details["msg_id"] != wantRef || ev.Details["related_reference"] != "MT202-REF-1" ||
		ev.Details["queried_kind"] != "mt202" || ev.Details["sender_lt"] != "DEUTDEFFXXX" {
		t.Errorf("details = %v, want msg/related/kind/sender", ev.Details)
	}
	if _, hasSchedule := ev.Details["schedule_id"]; hasSchedule {
		t.Errorf("details = %v, want no schedule_id on the MT202 tier", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (a query touches no account)", ev.Accounts)
	}
}

func TestExportMT295ForMT202UnknownMT202(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	opts := mt295ExportOptions(t)
	// No MT202 was ever registered under this reference: the
	// transfer could not have been sent, so the query parks
	// fail-closed instead of asking about nothing.
	res, err := l.ExportMT295ForMT202("MT202-REF-9", opts, "ledger-operator")
	if !errors.Is(err, ErrMT295UnknownMT202) {
		t.Fatalf("err = %v, want ErrMT295UnknownMT202", err)
	}
	if res.RepairID == "" {
		t.Error("repair ID empty, want the parked export recorded")
	}
	if res.QueriedKind != MT295QueriedMT202 {
		t.Errorf("parked queried kind = %q, want %q", res.QueriedKind, MT295QueriedMT202)
	}
	if res.RelatedReference != "MT202-REF-9" {
		t.Errorf("parked :21: = %q, want %q", res.RelatedReference, "MT202-REF-9")
	}
	// A parked export records nothing: no registry entry, no
	// audit marker.
	ref := mt295MessageReference("MT202-REF-9", opts.Queries, opts.Narrative, opts.Description)
	if _, ok := l.GetMT295Export(ref); ok {
		t.Error("GetMT295Export = true for a parked export, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 0 {
		t.Errorf("mt295_query_exported count = %d, want 0 for a parked export", countOp(events, "mt295_query_exported"))
	}
	// A re-sent export parks again.
	if _, err := l.ExportMT295ForMT202("MT202-REF-9", opts, "ledger-operator"); !errors.Is(err, ErrMT295UnknownMT202) {
		t.Errorf("re-sent err = %v, want ErrMT295UnknownMT202 again", err)
	}
}

func TestExportMT295ForMT202RepeatIsDuplicate(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	opts := mt295ExportOptions(t)
	first, err := l.ExportMT295ForMT202("MT202-REF-1", opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForMT202: %v", err)
	}
	second, err := l.ExportMT295ForMT202("MT202-REF-1", opts, "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT295ForMT202 repeat: %v", err)
	}
	if !second.Duplicate {
		t.Error("duplicate = false on repeat export")
	}
	if second.Message != first.Message {
		t.Errorf("repeat export differs:\nfirst:\n%s\nsecond:\n%s", first.Message, second.Message)
	}
	if second.QueriedKind != MT295QueriedMT202 {
		t.Errorf("repeat queried kind = %q, want %q", second.QueriedKind, MT295QueriedMT202)
	}
	// The replay emits no second audit event.
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_exported") != 1 {
		t.Errorf("mt295_query_exported count = %d, want 1 after replay", countOp(events, "mt295_query_exported"))
	}
}

func TestExportMT295ForMT202Conflict(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	opts := mt295ExportOptions(t)
	ref := mt295MessageReference("MT202-REF-1", opts.Queries, opts.Narrative, opts.Description)
	// A :20: already exported under different terms: the
	// reference names one export, so a second, different question
	// under it is refused rather than silently overwritten. The
	// derivation makes this unreachable through the public API
	// (the same terms hash to the same :20:), so the conflicting
	// record is seeded directly to exercise the gate itself.
	l.mt295Exports = map[string]mt295ExportRecord{
		ref: {
			QueriedRef:  "MT202-REF-1",
			QueriedKind: MT295QueriedMT202,
			Options: MT295ExportOptions{
				SenderLT:   "DEUTDEFF",
				ReceiverLT: "CHASUS33",
				Queries:    []string{"STAT"},
				Narrative:  []string{"A different question"},
			},
			Result: MT295ExportResult{Reference: ref, RelatedReference: "MT202-REF-1", QueriedKind: MT295QueriedMT202, Message: "seeded"},
		},
	}
	if _, err := l.ExportMT295ForMT202("MT202-REF-1", opts, "ledger-operator"); !errors.Is(err, ErrMT295MsgIDConflict) {
		t.Errorf("err = %v, want ErrMT295MsgIDConflict", err)
	}
}
