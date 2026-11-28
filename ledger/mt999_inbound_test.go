package ledger

import (
	"errors"
	"testing"
)

// mt999InboundLedger seeds a payout ledger with an audit log, the
// fixture for the ApplyMT999 tests: the outbound MT999 export
// needs no funded account (a free-format note moves nothing),
// and the payout anchor needs a submitted payout.
func mt999InboundLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

// mt999InboundDoc renders one inbound MT999 notification: ref as
// :20:, relatedRef as :21: (empty omits :21:), narrative as the
// :79: lines.
func mt999InboundDoc(t *testing.T, ref, relatedRef string, narrative []string) string {
	t.Helper()
	msg, err := ExportMT999(MT999{
		SenderLT:             "BANKDEFF",
		ReceiverLT:           "BANKUS33",
		TransactionReference: ref,
		RelatedReference:     relatedRef,
		Narrative:            narrative,
	})
	if err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	return msg
}

// applyMT999Doc parses doc and applies it, failing the test on
// parse errors: the gates under test are the apply gates, not the
// parse gates.
func applyMT999Doc(t *testing.T, l *Ledger, doc string) (MT999ApplyResult, error) {
	t.Helper()
	m, err := ParseMT999(doc)
	if err != nil {
		t.Fatalf("ParseMT999: %v", err)
	}
	return l.ApplyMT999(m, "correspondent-bank")
}

// exportMT999Outbound exports our outbound MT999 with :20: ref
// and returns the reference — the :21: an inbound notification
// answering it must name.
func exportMT999Outbound(t *testing.T, l *Ledger, ref string) string {
	t.Helper()
	m := mt999Base(t)
	m.TransactionReference = ref
	if _, err := l.ExportMT999(m, "ledger-operator"); err != nil {
		t.Fatalf("ExportMT999: %v", err)
	}
	return ref
}

func TestApplyMT999_AnswersOutboundExport(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	outboundRef := exportMT999Outbound(t, l, "FREE-999-OUT-1")
	before := l.Version()

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-1", outboundRef,
		[]string{"CASH POSITION CONFIRMED", "BALANCE AS EXPECTED"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "FREE-999-IN-1" {
		t.Errorf("reference = %q, want FREE-999-IN-1", res.Reference)
	}
	if res.RelatedReference != outboundRef || res.OutboundReference != outboundRef {
		t.Errorf("related/outbound = %q/%q, want %q", res.RelatedReference, res.OutboundReference, outboundRef)
	}
	if res.PayoutID != "" {
		t.Errorf("payout_id = %q, want empty (outbound anchor, not payout)", res.PayoutID)
	}
	if len(res.Narrative) != 2 || res.Narrative[0] != "CASH POSITION CONFIRMED" ||
		res.Narrative[1] != "BALANCE AS EXPECTED" {
		t.Errorf("narrative = %v, want the correspondent's lines verbatim", res.Narrative)
	}
	// Registering is read-only: no version bump.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	// The stored notification round-trips through the getter.
	stored, ok := l.GetMT999Received("FREE-999-IN-1")
	if !ok {
		t.Fatal("GetMT999Received = false, want the stored notification")
	}
	if stored.OutboundReference != outboundRef || len(stored.Narrative) != 2 {
		t.Errorf("stored = %+v, want the registered notification", stored)
	}
	if _, ok := l.GetMT999Received("no-such-notification"); ok {
		t.Error("GetMT999Received unknown = true, want false")
	}
	// Audit: exactly one received marker, with the exact details
	// — the :20: on the durable record, and no accounts: a
	// free-format note touches no ledger account.
	events := readAuditOps(t, al)
	if countOp(events, "mt999_received") != 1 {
		t.Errorf("mt999_received count = %d, want 1", countOp(events, "mt999_received"))
	}
	ev := findOp(events, "mt999_received")
	if ev == nil {
		t.Fatal("mt999_received event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT999)", ev.Actor)
	}
	if ev.TraceID != "FREE-999-IN-1" {
		t.Errorf("trace = %q, want FREE-999-IN-1", ev.TraceID)
	}
	if ev.Details["msg_id"] != "FREE-999-IN-1" || ev.Details["related_reference"] != outboundRef ||
		ev.Details["sender_lt"] != "BANKDEFFXXX" {
		t.Errorf("details = %v, want msg/related/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only registration)", ev.Accounts)
	}
}

func TestApplyMT999_AnchorsPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt999-apply-2", 250_000)

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-2", p.ID,
		[]string{"CHARGES FOR PAYMENT", "WILL BE ADVISED SEPARATELY"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.OutboundReference != "" {
		t.Errorf("outbound_reference = %q, want empty (payout anchor, not outbound)", res.OutboundReference)
	}
	if res.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", res.PayoutID, p.ID)
	}
	if res.RelatedReference != p.ID {
		t.Errorf("related_reference = %q, want %q", res.RelatedReference, p.ID)
	}
	stored, ok := l.GetMT999Received("FREE-999-IN-2")
	if !ok || stored.PayoutID != p.ID {
		t.Errorf("stored = %+v, want the payout-anchored notification", stored)
	}
}

func TestApplyMT999_EmptyRelatedReferenceRegistersStandalone(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-3", "",
		[]string{"GENERAL CASH MANAGEMENT NOTE"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.OutboundReference != "" || res.PayoutID != "" {
		t.Errorf("outbound/payout = %q/%q, want both empty (standalone)", res.OutboundReference, res.PayoutID)
	}
	if res.RelatedReference != "" {
		t.Errorf("related_reference = %q, want empty", res.RelatedReference)
	}
	stored, ok := l.GetMT999Received("FREE-999-IN-3")
	if !ok || len(stored.Narrative) != 1 || stored.Narrative[0] != "GENERAL CASH MANAGEMENT NOTE" {
		t.Errorf("stored = %+v, want the standalone notification", stored)
	}
	if countOp(readAuditOps(t, al), "mt999_received") != 1 {
		t.Error("standalone notification did not emit exactly one mt999_received event")
	}
}

func TestApplyMT999_UnknownRelatedReferenceGoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-4", "no-such-anchor",
		[]string{"ANSWER TO NOTHING WE SENT"}))
	if !errors.Is(err, ErrMT999UnknownReference) {
		t.Errorf("err = %v, want ErrMT999UnknownReference", err)
	}
	// The parked result carries the repair ID alongside the
	// error: the notification is visible to an operator, not
	// dropped.
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
	if res.Reference != "FREE-999-IN-4" || res.RelatedReference != "no-such-anchor" {
		t.Errorf("parked result = %+v, want reference + related_reference", res)
	}
	rec, ok := l.GetRepair(res.RepairID)
	if !ok {
		t.Fatalf("GetRepair(%q) = false, want the parked record", res.RepairID)
	}
	if rec.ReasonCode != RepairReasonUnknownRoute {
		t.Errorf("repair reason = %q, want unknown_route", rec.ReasonCode)
	}
	if rec.Instruction.ID != "no-such-anchor" {
		t.Errorf("repair instruction id = %q, want the unanchored :21:", rec.Instruction.ID)
	}
	if rec.Instruction.Destination != "mt999-inbound:FREE-999-IN-4" {
		t.Errorf("repair destination = %q, want mt999-inbound:FREE-999-IN-4", rec.Instruction.Destination)
	}
	// No notification was recorded: nothing to replay, and no
	// received marker event — the repair_enqueued event is the
	// record.
	if _, ok := l.GetMT999Received("FREE-999-IN-4"); ok {
		t.Error("GetMT999Received = true for a parked notification, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt999_received") != 0 {
		t.Errorf("mt999_received count = %d, want 0", countOp(events, "mt999_received"))
	}
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
}

func TestApplyMT999_ParkedThenRegistersOnceAnchorExists(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	doc := mt999InboundDoc(t, "FREE-999-IN-5", "FREE-999-LATE-1",
		[]string{"ANSWER ARRIVED EARLY"})

	// The anchor does not exist yet: the notification parks.
	if _, err := applyMT999Doc(t, l, doc); !errors.Is(err, ErrMT999UnknownReference) {
		t.Fatalf("first apply err = %v, want ErrMT999UnknownReference", err)
	}
	// The outbound message lands afterwards: the same
	// notification now registers instead of parking again.
	exportMT999Outbound(t, l, "FREE-999-LATE-1")
	res, err := applyMT999Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT999 after anchor exists: %v", err)
	}
	if res.OutboundReference != "FREE-999-LATE-1" {
		t.Errorf("outbound_reference = %q, want FREE-999-LATE-1", res.OutboundReference)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first registration")
	}
	// Exactly one repair event (the first, parked apply) and
	// exactly one received marker (the second, registered
	// apply).
	events := readAuditOps(t, al)
	if countOp(events, "repair_enqueued") != 1 {
		t.Errorf("repair_enqueued count = %d, want 1", countOp(events, "repair_enqueued"))
	}
	if countOp(events, "mt999_received") != 1 {
		t.Errorf("mt999_received count = %d, want 1", countOp(events, "mt999_received"))
	}
}

func TestApplyMT999_ReplaysSameTerms(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	outboundRef := exportMT999Outbound(t, l, "FREE-999-OUT-6")
	doc := mt999InboundDoc(t, "FREE-999-IN-6", outboundRef,
		[]string{"CASH POSITION CONFIRMED"})

	first, err := applyMT999Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	dup, err := applyMT999Doc(t, l, doc)
	if err != nil {
		t.Fatalf("ApplyMT999 replay: %v", err)
	}
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if dup.OutboundReference != first.OutboundReference || len(dup.Narrative) != len(first.Narrative) {
		t.Error("replay outcome differs from the stored outcome")
	}
	if countOp(readAuditOps(t, al), "mt999_received") != 1 {
		t.Error("replay emitted a second mt999_received event, want exactly 1")
	}
}

func TestApplyMT999_ConflictOnDifferentTerms(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	outboundRef := exportMT999Outbound(t, l, "FREE-999-OUT-7")

	if _, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-7", outboundRef,
		[]string{"CASH POSITION CONFIRMED"})); err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	// Same :20:, different narrative: a conflict, never a
	// silent overwrite — and the stored notification is
	// untouched.
	if _, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-7", outboundRef,
		[]string{"A different note"})); !errors.Is(err, ErrMT999MsgIDConflict) {
		t.Errorf("conflict err = %v, want ErrMT999MsgIDConflict", err)
	}
	// Same :20:, different :21:: the same conflict — the :20:
	// names one notification.
	if _, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-7", "",
		[]string{"CASH POSITION CONFIRMED"})); !errors.Is(err, ErrMT999MsgIDConflict) {
		t.Errorf("anchor-change err = %v, want ErrMT999MsgIDConflict", err)
	}
	stored, ok := l.GetMT999Received("FREE-999-IN-7")
	if !ok || len(stored.Narrative) != 1 || stored.Narrative[0] != "CASH POSITION CONFIRMED" {
		t.Errorf("stored = %+v, want the first notification untouched", stored)
	}
	if countOp(readAuditOps(t, al), "mt999_received") != 1 {
		t.Error("conflicting apply emitted a second mt999_received event, want exactly 1")
	}
}

func TestApplyMT999_OutboundAnchorBeatsPayout(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	p := mt103SubmittedPayout(t, l, "po-mt999-both", 250_000)
	// The same string names both a payout and our outbound
	// MT999's :20:: the outbound tier wins — the :21: is a
	// back-reference first, a payout anchor second.
	exportMT999Outbound(t, l, p.ID)

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-8", p.ID,
		[]string{"ANSWER TO OUR NOTE"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.OutboundReference != p.ID {
		t.Errorf("outbound_reference = %q, want %q", res.OutboundReference, p.ID)
	}
	if res.PayoutID != "" {
		t.Errorf("payout_id = %q, want empty (outbound tier wins)", res.PayoutID)
	}
}

func TestApplyMT999_RejectsInvalidMessage(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)

	// No :79: narrative: a free-format message with nothing to
	// say is not a message.
	if _, err := l.ApplyMT999(MT999{
		SenderLT:             "BANKDEFF",
		ReceiverLT:           "BANKUS33",
		TransactionReference: "FREE-999-BAD-1",
	}, "correspondent-bank"); !errors.Is(err, ErrMT999Narrative) {
		t.Errorf("empty narrative err = %v, want ErrMT999Narrative", err)
	}
	// A :20: over 16 characters is refused at the apply gate,
	// so hand-built requests share the parse gate.
	if _, err := l.ApplyMT999(MT999{
		SenderLT:             "BANKDEFF",
		ReceiverLT:           "BANKUS33",
		TransactionReference: "FREE-999-REFERENCE-TOO-LONG",
		Narrative:            []string{"A NOTE"},
	}, "correspondent-bank"); !errors.Is(err, ErrMT999Reference) {
		t.Errorf("long :20: err = %v, want ErrMT999Reference", err)
	}
	// Nothing was recorded for either refusal.
	if _, ok := l.GetMT999Received("FREE-999-BAD-1"); ok {
		t.Error("invalid apply recorded a notification, want none")
	}
	if countOp(readAuditOps(t, al), "mt999_received") != 0 {
		t.Error("invalid apply emitted mt999_received, want none")
	}
}

func TestApplyMT999_GetterReturnsNarrativeVerbatim(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)

	// A :79: continuation line may itself start with a colon
	// without being mistaken for a new field — the verbatim
	// carry must survive the round-trip through the getter.
	want := []string{"CASH POSITION AS OF TODAY", ":SECOND LOOKS LIKE A FIELD"}
	if _, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-9", "", want)); err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	stored, ok := l.GetMT999Received("FREE-999-IN-9")
	if !ok {
		t.Fatal("GetMT999Received = false, want the stored notification")
	}
	if len(stored.Narrative) != len(want) {
		t.Fatalf("narrative = %v, want %v", stored.Narrative, want)
	}
	for i, ln := range want {
		if stored.Narrative[i] != ln {
			t.Errorf("narrative[%d] = %q, want %q", i, stored.Narrative[i], ln)
		}
	}
	// The returned slices are copies: mutating them cannot
	// corrupt the registry.
	stored.Narrative[0] = "TAMPERED"
	again, ok := l.GetMT999Received("FREE-999-IN-9")
	if !ok || again.Narrative[0] != "CASH POSITION AS OF TODAY" {
		t.Errorf("getter returned a live slice, want a copy: %+v", again)
	}
}

func TestApplyMT999_AnchorsInboundMT202(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ApplyLedger(t, al)
	if _, err := l.ApplyMT202(mt202ApplyMsg("MT202-999-IN-1"), ""); err != nil {
		t.Fatalf("ApplyMT202 setup: %v", err)
	}

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-10", "MT202-999-IN-1",
		[]string{"CHARGES FOR TRANSFER MT202-999-IN-1"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.MT202Ref != "MT202-999-IN-1" {
		t.Errorf("mt202_ref = %q, want MT202-999-IN-1", res.MT202Ref)
	}
	if res.OutboundReference != "" || res.PayoutID != "" {
		t.Errorf("outbound/payout = %q/%q, want both empty (MT202 anchor)", res.OutboundReference, res.PayoutID)
	}
	// The stored notification round-trips through the getter
	// with the MT202 anchor attached.
	stored, ok := l.GetMT999Received("FREE-999-IN-10")
	if !ok {
		t.Fatal("GetMT999Received = false, want the stored notification")
	}
	if stored.MT202Ref != "MT202-999-IN-1" {
		t.Errorf("stored mt202_ref = %q, want MT202-999-IN-1", stored.MT202Ref)
	}
	// Exactly one received marker: the MT202 anchor reuses the
	// mt999_received audit event, no new action.
	if countOp(readAuditOps(t, al), "mt999_received") != 1 {
		t.Error("mt999_received count != 1, want exactly one marker")
	}
}

func TestApplyMT999_AnchorsOutboundMT202(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered setup: %v", err)
	}

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-11", "MT202-REF-1",
		[]string{"CONFIRMING OUR TRANSFER MT202-REF-1"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.MT202Ref != "MT202-REF-1" {
		t.Errorf("mt202_ref = %q, want MT202-REF-1", res.MT202Ref)
	}
	if res.OutboundReference != "" || res.PayoutID != "" {
		t.Errorf("outbound/payout = %q/%q, want both empty (MT202 anchor)", res.OutboundReference, res.PayoutID)
	}
	if countOp(readAuditOps(t, al), "mt999_received") != 1 {
		t.Error("mt999_received count != 1, want exactly one marker")
	}
}

func TestApplyMT999_UnregisteredMT202GoesToRepair(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)

	// An MT202-shaped :21: with no registration in either
	// tier is still unknown: the MT202 tier is a lookup, not
	// a fallback that accepts any :21: shaped like a
	// transfer reference.
	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-12", "MT202-UNKNOWN-1",
		[]string{"ANSWER TO NOTHING WE SENT"}))
	if !errors.Is(err, ErrMT999UnknownReference) {
		t.Errorf("err = %v, want ErrMT999UnknownReference", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id empty, want the parked repair record")
	}
	if _, ok := l.GetMT999Received("FREE-999-IN-12"); ok {
		t.Error("GetMT999Received = true for a parked notification, want false")
	}
}

func TestApplyMT999_PayoutBeatsMT202(t *testing.T) {
	al := mustAuditLog(t)
	l := mt999InboundLedger(t, al)
	// The MT202 apply needs the funded nostro accounts the
	// payout fixture does not seed.
	if _, _, err := l.Post(JournalEntry{ID: "fund-nostro", DebitAccount: "nostro-acct", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.RegisterNostroAccount("nostro-acct", "DEUTDEFF", NostroKindNostro); err != nil {
		t.Fatalf("RegisterNostroAccount nostro: %v", err)
	}
	if _, err := l.RegisterNostroAccount("vostro-acct", "BNPAFRPP", NostroKindVostro); err != nil {
		t.Fatalf("RegisterNostroAccount vostro: %v", err)
	}
	p := mt103SubmittedPayout(t, l, "po-mt999-both", 250_000)
	// The same string names both a payout and an applied
	// inbound MT202's :20:: the payout tier wins — the :21:
	// is a payout anchor before it is a transfer anchor.
	if _, err := l.ApplyMT202(mt202ApplyMsg(p.ID), ""); err != nil {
		t.Fatalf("ApplyMT202 setup: %v", err)
	}

	res, err := applyMT999Doc(t, l, mt999InboundDoc(t, "FREE-999-IN-13", p.ID,
		[]string{"CHARGES FOR PAYMENT"}))
	if err != nil {
		t.Fatalf("ApplyMT999: %v", err)
	}
	if res.PayoutID != p.ID {
		t.Errorf("payout_id = %q, want %q", res.PayoutID, p.ID)
	}
	if res.MT202Ref != "" {
		t.Errorf("mt202_ref = %q, want empty (payout tier wins)", res.MT202Ref)
	}
	if res.OutboundReference != "" {
		t.Errorf("outbound_reference = %q, want empty (payout tier wins)", res.OutboundReference)
	}
}
