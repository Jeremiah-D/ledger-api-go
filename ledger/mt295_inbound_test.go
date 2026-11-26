package ledger

import (
	"errors"
	"strings"
	"testing"
)

// mt295InboundLedger seeds a payout ledger with an audit log, the
// fixture for the ApplyMT295 tests: registering a query moves
// nothing, so no funded account is needed.
func mt295InboundLedger(t *testing.T, al *AuditLog) *Ledger {
	t.Helper()
	return seedPayoutLedger(t, WithAuditLog(al))
}

// mt295InboundDoc renders one inbound MT295 query: ref as :20:,
// relatedRef as :21: (the queried message's :20:), queries as the
// :75: lines, narrative as the :77A: lines (empty omits :77A:),
// description as the :79: lines (empty omits :79:). There is no
// ExportMT295 (LG-466 explicitly excludes an MT295 export —
// this ledger never asks another bank about a Category 2 message
// it sent), so the fixture is hand-built FIN text, the MT292
// inbound convention.
func mt295InboundDoc(ref, relatedRef string, queries, narrative, description []string) string {
	var b strings.Builder
	b.WriteString("{1:F01CITIUS33XXX0000000000}\n")
	b.WriteString("{2:I295NWBKGB2LXXXU3003}\n")
	b.WriteString("{3:{108:" + ref + "}}\n")
	b.WriteString("{4:\n")
	b.WriteString(":20:" + ref + "\n")
	b.WriteString(":21:" + relatedRef + "\n")
	b.WriteString(":75:" + queries[0] + "\n")
	for _, ln := range queries[1:] {
		b.WriteString(ln + "\n")
	}
	if len(narrative) > 0 {
		b.WriteString(":77A:" + narrative[0] + "\n")
		for _, ln := range narrative[1:] {
			b.WriteString(ln + "\n")
		}
	}
	if len(description) > 0 {
		b.WriteString(":79:" + description[0] + "\n")
		for _, ln := range description[1:] {
			b.WriteString(ln + "\n")
		}
	}
	b.WriteString("-}")
	return b.String()
}

// mt295OutboundMT292Ref is a :21: with the shape of an outbound
// MT292 :20: — the deterministic reference for schedule
// "sched-1", run 1 (mt292MessageReference). A fresh fixture
// schedule's first run is run 0, so this is not what a fresh
// fixture export registers — it is the shape-only ref the
// unregistered-shape test parks on. Registered tests use the
// export result's Reference instead (the mt296MT292ExportRef
// precedent, LG-473).
func mt295OutboundMT292Ref() string {
	return mt296AnsweredRef()
}

// applyInboundMT295 parses and applies one inbound MT295 for the
// fixture, the inbound-apply test convention.
func applyInboundMT295(t *testing.T, l *Ledger, doc, actor string) MT295ApplyResult {
	t.Helper()
	m, err := ParseMT295(doc)
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	res, err := l.ApplyMT295(m, actor)
	if err != nil {
		t.Fatalf("ApplyMT295: %v", err)
	}
	return res
}

// registerInboundMT210ForQuery registers one inbound MT210
// notification on a ledger with bob's IBAN, the notification a
// later MT295 query asks about.
func registerInboundMT210ForQuery(t *testing.T, l *Ledger, msgID string) ExpectedReceipt {
	t.Helper()
	doc := mt210InboundDoc(msgID, "GB29NWBK60161331926819", "261124", "sched-1/run/1", "USD100,00", ":52A:DEUTDEFF")
	return applyInboundMT210(t, l, doc, "ops").Receipt
}

func TestParseMT295_FullDocument(t *testing.T) {
	doc := mt295InboundDoc("QRY-295-1", "NTF-295-210",
		[]string{"STAT", "PLEASE ADVISE STATUS"},
		[]string{"OPERATOR NOTE"},
		[]string{"/REJT/ NOTED BY SENDER", "BANK ASKS FOR THE VALUE DATE"})
	m, err := ParseMT295(doc)
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	if m.TransactionReference != "QRY-295-1" || m.RelatedReference != "NTF-295-210" {
		t.Errorf("refs = %q/%q, want QRY-295-1/NTF-295-210", m.TransactionReference, m.RelatedReference)
	}
	if len(m.Queries) != 2 || m.Queries[0] != "STAT" || m.Queries[1] != "PLEASE ADVISE STATUS" {
		t.Errorf("queries = %v, want the :75: lines verbatim", m.Queries)
	}
	if len(m.Narrative) != 1 || m.Narrative[0] != "OPERATOR NOTE" {
		t.Errorf("narrative = %v, want [OPERATOR NOTE]", m.Narrative)
	}
	if len(m.Description) != 2 || m.Description[0] != "/REJT/ NOTED BY SENDER" ||
		m.Description[1] != "BANK ASKS FOR THE VALUE DATE" {
		t.Errorf("description = %v, want the :79: lines verbatim", m.Description)
	}
	if m.SenderLT != "CITIUS33XXX" || m.ReceiverLT != "NWBKGB2LXXX" {
		t.Errorf("LTs = %q/%q, want CITIUS33XXX/NWBKGB2LXXX", m.SenderLT, m.ReceiverLT)
	}
	// CRLF endings and a trailing newline after the trailer are
	// tolerated, like the MT296 parse.
	crlf := strings.ReplaceAll(doc, "\n", "\r\n") + "\n"
	if _, err := ParseMT295(crlf); err != nil {
		t.Errorf("ParseMT295 CRLF: %v", err)
	}
	// A :79: continuation line may itself start with a colon —
	// the MT199 rule — so a quoted tag inside the description is
	// content, not a new field.
	colonDoc := mt295InboundDoc("QRY-295-1C", "NTF-295-210",
		[]string{"STAT"}, nil,
		[]string{":11S: QUOTED IN PROSE", "SECOND LINE"})
	m, err = ParseMT295(colonDoc)
	if err != nil {
		t.Fatalf("ParseMT295 colon continuation: %v", err)
	}
	if len(m.Description) != 2 || m.Description[0] != ":11S: QUOTED IN PROSE" {
		t.Errorf("description = %v, want the colon line kept as content", m.Description)
	}
}

func TestParseMT295_MinimalDocument(t *testing.T) {
	m, err := ParseMT295(mt295InboundDoc("QRY-295-2", "NTF-295-210",
		[]string{"DUPL"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	if len(m.Queries) != 1 || m.Queries[0] != "DUPL" {
		t.Errorf("queries = %v, want [DUPL]", m.Queries)
	}
	if m.Narrative != nil || m.Description != nil {
		t.Errorf("narrative/description = %v/%v, want nil (both optional)", m.Narrative, m.Description)
	}
}

func TestParseMT295_MissingQueriesRejected(t *testing.T) {
	// :75: is mandatory — the handbook's M, not the MT195
	// module's optional call — so a query with no question text
	// at all is malformed, even with a :79: description.
	doc := "{1:F01CITIUS33XXX0000000000}\n" +
		"{2:I295NWBKGB2LXXXU3003}\n" +
		"{3:{108:QRY-295-3}}\n" +
		"{4:\n" +
		":20:QRY-295-3\n" +
		":21:NTF-295-210\n" +
		":77A:OPERATOR NOTE\n" +
		":79:PLEASE ADVISE\n" +
		"-}"
	if _, err := ParseMT295(doc); !errors.Is(err, ErrMT295Field) {
		t.Errorf("ParseMT295 no :75:: err = %v, want ErrMT295Field", err)
	}
}

func TestParseMT295_MissingRelatedReferenceRejected(t *testing.T) {
	// A query with no :21: names nothing to ask about.
	doc := "{1:F01CITIUS33XXX0000000000}\n" +
		"{2:I295NWBKGB2LXXXU3003}\n" +
		"{3:{108:QRY-295-4}}\n" +
		"{4:\n" +
		":20:QRY-295-4\n" +
		":75:STAT\n" +
		":77A:OPERATOR NOTE\n" +
		"-}"
	if _, err := ParseMT295(doc); !errors.Is(err, ErrMT295Field) {
		t.Errorf("ParseMT295 no :21:: err = %v, want ErrMT295Field", err)
	}
}

func TestParseMT295_UnknownFieldRejected(t *testing.T) {
	// :11S: is outside the core set — the queried message is
	// already named by the :21:, and a second copy of its type
	// and date would be a different claim.
	doc := mt295InboundDoc("QRY-295-5", "NTF-295-210",
		[]string{"STAT"}, nil, nil)
	doc = strings.Replace(doc, "-}", ":11S:210261124\n-}", 1)
	if _, err := ParseMT295(doc); !errors.Is(err, ErrMT295Field) {
		t.Errorf("ParseMT295 :11S:: err = %v, want ErrMT295Field", err)
	}
	// A duplicate :75: is an unexpected field, not a
	// continuation.
	dupDoc := mt295InboundDoc("QRY-295-5D", "NTF-295-210",
		[]string{"STAT"}, nil, nil)
	dupDoc = strings.Replace(dupDoc, "-}", ":75:DUPL\n-}", 1)
	if _, err := ParseMT295(dupDoc); !errors.Is(err, ErrMT295Field) {
		t.Errorf("ParseMT295 duplicate :75:: err = %v, want ErrMT295Field", err)
	}
}

func TestParseMT295_MURMismatch(t *testing.T) {
	doc := mt295InboundDoc("QRY-295-6", "NTF-295-210",
		[]string{"STAT"}, nil, nil)
	doc = strings.Replace(doc, "{3:{108:QRY-295-6}}", "{3:{108:OTHER-REF}}", 1)
	if _, err := ParseMT295(doc); !errors.Is(err, ErrMT295Framing) {
		t.Errorf("ParseMT295 MUR mismatch: err = %v, want ErrMT295Framing", err)
	}
}

func TestApplyMT295_AnchorsMT210(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	receipt := registerInboundMT210ForQuery(t, l, "NTF-295-210")
	if receipt.MsgID != "NTF-295-210" {
		t.Fatalf("receipt MsgID = %q, want NTF-295-210", receipt.MsgID)
	}
	before := l.Version()
	entriesBefore := len(l.Entries())

	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-10", "NTF-295-210",
		[]string{"STAT", "PLEASE ADVISE STATUS"},
		[]string{"OPERATOR NOTE"},
		[]string{"BANK ASKS FOR THE VALUE DATE"}), "correspondent-bank")
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.Reference != "QRY-295-10" || res.RelatedReference != "NTF-295-210" {
		t.Errorf("refs = %q/%q, want QRY-295-10/NTF-295-210", res.Reference, res.RelatedReference)
	}
	if res.AnchoredKind != MT295AnchorMT210 {
		t.Errorf("anchored_kind = %q, want mt210", res.AnchoredKind)
	}
	if len(res.Queries) != 2 || res.Queries[0] != "STAT" {
		t.Errorf("queries = %v, want the :75: lines verbatim", res.Queries)
	}
	if len(res.Narrative) != 1 || res.Narrative[0] != "OPERATOR NOTE" {
		t.Errorf("narrative = %v, want the :77A: lines verbatim", res.Narrative)
	}
	if len(res.Description) != 1 || res.Description[0] != "BANK ASKS FOR THE VALUE DATE" {
		t.Errorf("description = %v, want the :79: lines verbatim", res.Description)
	}
	// Registering is read-only: no version bump, no journal rows.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("entries = %d, want %d (no journal rows)", len(l.Entries()), entriesBefore)
	}
	// The stored query round-trips through the getter.
	stored, ok := l.GetMT295Received("QRY-295-10")
	if !ok {
		t.Fatal("GetMT295Received = false, want the stored query")
	}
	if stored.RelatedReference != "NTF-295-210" || stored.AnchoredKind != MT295AnchorMT210 ||
		len(stored.Queries) != 2 || len(stored.Narrative) != 1 || len(stored.Description) != 1 {
		t.Errorf("stored = %+v, want the registered query", stored)
	}
	if _, ok := l.GetMT295Received("no-such-query"); ok {
		t.Error("GetMT295Received unknown = true, want false")
	}
	// Audit: exactly one received marker, with the exact details
	// — the :20: on the durable record, and no accounts: a
	// query touches no ledger account.
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_received") != 1 {
		t.Errorf("mt295_query_received count = %d, want 1", countOp(events, "mt295_query_received"))
	}
	ev := findOp(events, "mt295_query_received")
	if ev == nil {
		t.Fatal("mt295_query_received event missing")
	}
	if ev.Actor != "correspondent-bank" {
		t.Errorf("actor = %q, want correspondent-bank (ApplyMT295)", ev.Actor)
	}
	if ev.TraceID != "QRY-295-10" {
		t.Errorf("trace = %q, want QRY-295-10", ev.TraceID)
	}
	if ev.Details["msg_id"] != "QRY-295-10" || ev.Details["related_reference"] != "NTF-295-210" ||
		ev.Details["sender_lt"] != "CITIUS33XXX" {
		t.Errorf("details = %v, want msg/related/sender", ev.Details)
	}
	if len(ev.Accounts) != 0 {
		t.Errorf("accounts = %v, want none (evidence-only registration)", ev.Accounts)
	}
}

func TestApplyMT295_AnchorsMT292(t *testing.T) {
	l, _ := mt210TestLedger(t)
	registerInboundMT210ForQuery(t, l, "NTF-295-211")
	// The inbound MT292 cancels that notification; its :20: is
	// the second anchor tier.
	applyInboundMT292(t, l, mt292InboundDoc("CXL-295-292", "NTF-295-211", "210261110",
		"Client withdrew the order"), "ops")
	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-11", "CXL-295-292",
		[]string{"STAT"}, nil, nil), "ops")
	if res.AnchoredKind != MT295AnchorMT292 {
		t.Errorf("anchored_kind = %q, want mt292", res.AnchoredKind)
	}
	if res.RelatedReference != "CXL-295-292" {
		t.Errorf("related_reference = %q, want CXL-295-292", res.RelatedReference)
	}
}

func TestApplyMT295_AnchorsMT202(t *testing.T) {
	al := mustAuditLog(t)
	l := mt202ApplyLedger(t, al)
	m := mt202ApplyMsg("MT202-295-01")
	if res, err := l.ApplyMT202(m, ""); err != nil || res.Err != nil {
		t.Fatalf("ApplyMT202: err=%v res.Err=%v", err, res.Err)
	}
	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-12", "MT202-295-01",
		[]string{"STAT"}, nil, nil), "ops")
	if res.AnchoredKind != MT295AnchorMT202 {
		t.Errorf("anchored_kind = %q, want mt202", res.AnchoredKind)
	}
}

func TestApplyMT295_UnknownReferenceParks(t *testing.T) {
	al := mustAuditLog(t)
	l := mt295InboundLedger(t, al)
	m, err := ParseMT295(mt295InboundDoc("QRY-295-13", "NO-SUCH-MSG",
		[]string{"STAT"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	res, err := l.ApplyMT295(m, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("ApplyMT295 unknown :21:: err = %v, want ErrMT295UnknownReference", err)
	}
	if res.RepairID == "" {
		t.Error("RepairID empty on a parked query, want the repair record")
	}
	if res.AnchoredKind != "" {
		t.Errorf("anchored_kind = %q, want empty on a parked query", res.AnchoredKind)
	}
	// A parked query records nothing: no getter hit, no audit
	// marker — and a re-sent query parks again.
	if _, ok := l.GetMT295Received("QRY-295-13"); ok {
		t.Error("GetMT295Received = true for a parked query, want false")
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_received") != 0 {
		t.Errorf("mt295_query_received count = %d, want 0 (parked, not registered)", countOp(events, "mt295_query_received"))
	}
	if _, err := l.ApplyMT295(m, "ops"); !errors.Is(err, ErrMT295UnknownReference) {
		t.Errorf("re-sent parked query: err = %v, want ErrMT295UnknownReference again", err)
	}
}

func TestApplyMT295_OutboundMT292ShapeAlsoParks(t *testing.T) {
	// A :21: with the shape of our outbound MT292's :20: anchors
	// once the export is registered in mt292Exports (LG-474):
	// the query registers against the outbound cancellation
	// request, with kind outbound-mt292. (Before LG-474 the
	// shape alone parked — the outbound-shape asymmetry, now
	// closed by the registry lookup.)
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	exp, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-14", exp.Reference,
		[]string{"STAT"}, nil, nil), "ops")
	if res.AnchoredKind != MT295AnchorOutboundMT292 {
		t.Errorf("anchored_kind = %q, want outbound-mt292", res.AnchoredKind)
	}
	if res.RelatedReference != exp.Reference {
		t.Errorf("related_reference = %q, want %q", res.RelatedReference, exp.Reference)
	}
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	// The stored query round-trips through the getter, kind
	// included.
	stored, ok := l.GetMT295Received("QRY-295-14")
	if !ok {
		t.Fatal("GetMT295Received = false, want the stored query")
	}
	if stored.AnchoredKind != MT295AnchorOutboundMT292 {
		t.Errorf("stored anchored_kind = %q, want outbound-mt292", stored.AnchoredKind)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_received") != 1 {
		t.Errorf("mt295_query_received count = %d, want 1", countOp(events, "mt295_query_received"))
	}
}

func TestApplyMT295_OutboundMT292ShapeUnregisteredParks(t *testing.T) {
	// The shape alone is not enough (LG-474): a :21: shaped
	// like our outbound MT292's :20: that names no rendered
	// MT292 still parks fail-closed, with the repair ID on the
	// returned result.
	l := mt295InboundLedger(t, mustAuditLog(t))
	m, err := ParseMT295(mt295InboundDoc("QRY-295-17", mt295OutboundMT292Ref(),
		[]string{"STAT"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	res, err := l.ApplyMT295(m, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("ApplyMT295 unregistered outbound-shaped :21:: err = %v, want ErrMT295UnknownReference", err)
	}
	if res.RepairID == "" {
		t.Error("RepairID empty on a parked query, want the repair record")
	}
	if res.AnchoredKind != "" {
		t.Errorf("anchored_kind = %q, want empty on a parked query", res.AnchoredKind)
	}
}

func TestApplyMT295_AnchorsOutboundMT202(t *testing.T) {
	// An outbound MT202 :20: is caller-supplied, 1-16 `x`
	// characters with no shape to check, so the mt202Exports
	// lookup alone anchors it (LG-474, along the MT296 third
	// tier, LG-472).
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-18", "MT202-REF-1",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, []string{"PLEASE ADVISE"}), "ops")
	if res.AnchoredKind != MT295AnchorOutboundMT202 {
		t.Errorf("anchored_kind = %q, want outbound-mt202", res.AnchoredKind)
	}
	if res.RelatedReference != "MT202-REF-1" {
		t.Errorf("related_reference = %q, want MT202-REF-1", res.RelatedReference)
	}
	if len(res.Queries) != 1 || res.Queries[0] != "STAT" ||
		len(res.Narrative) != 1 || res.Narrative[0] != "OPERATOR NOTE" ||
		len(res.Description) != 1 || res.Description[0] != "PLEASE ADVISE" {
		t.Errorf("registered query = %+v, want the verbatim terms", res)
	}
}

func TestApplyMT295_UnregisteredMT202Parks(t *testing.T) {
	// A :21: naming no rendered MT202 transfer parks, even
	// though nothing about its shape could rule it out — the
	// registry is the whole anchor.
	l := mt295InboundLedger(t, mustAuditLog(t))
	m, err := ParseMT295(mt295InboundDoc("QRY-295-19", "MT202-REF-9",
		[]string{"STAT"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	res, err := l.ApplyMT295(m, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("ApplyMT295 unregistered MT202 :21:: err = %v, want ErrMT295UnknownReference", err)
	}
	if res.RepairID == "" {
		t.Error("RepairID empty on a parked query, want the repair record")
	}
}

func TestApplyMT295_AnchorsOutboundMT296(t *testing.T) {
	// An outbound MT296 :20: is operator-supplied, 1-16 `x`
	// characters with no shape to check, so the mt296Exports
	// lookup alone anchors it (LG-478, along the MT202 tier,
	// LG-474, and the MT296 third tier, LG-472).
	l, al, mt292Ref, _ := mt296ExportLedger(t)
	if _, err := l.ExportMT296ForAnswered(mt292Ref, mt296ExportOptions(t), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT296ForAnswered: %v", err)
	}
	before := l.Version()
	entriesBefore := len(l.Entries())

	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-22", "ANS-296-0001",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, []string{"PLEASE ADVISE"}), "ops")
	if res.Duplicate {
		t.Error("duplicate = true on first apply")
	}
	if res.AnchoredKind != MT295AnchorOutboundMT296 {
		t.Errorf("anchored_kind = %q, want outbound-mt296", res.AnchoredKind)
	}
	if res.Reference != "QRY-295-22" || res.RelatedReference != "ANS-296-0001" {
		t.Errorf("refs = %q/%q, want QRY-295-22/ANS-296-0001", res.Reference, res.RelatedReference)
	}
	if len(res.Queries) != 1 || res.Queries[0] != "STAT" ||
		len(res.Narrative) != 1 || res.Narrative[0] != "OPERATOR NOTE" ||
		len(res.Description) != 1 || res.Description[0] != "PLEASE ADVISE" {
		t.Errorf("registered query = %+v, want the verbatim terms", res)
	}
	// Registering is read-only: no version bump, no journal rows.
	if l.Version() != before {
		t.Errorf("version = %d, want %d (read-only, no bump)", l.Version(), before)
	}
	if len(l.Entries()) != entriesBefore {
		t.Errorf("entries = %d, want %d (no journal rows)", len(l.Entries()), entriesBefore)
	}
	stored, ok := l.GetMT295Received("QRY-295-22")
	if !ok {
		t.Fatal("GetMT295Received = false, want the stored query")
	}
	if stored.RelatedReference != "ANS-296-0001" || stored.AnchoredKind != MT295AnchorOutboundMT296 {
		t.Errorf("stored = %+v, want the registered query", stored)
	}
	events := readAuditOps(t, al)
	// Two markers: the fixture's own query (QRY-296-295) and
	// this one — readAuditOps closes the log, so the count is
	// asserted once, at the end.
	if countOp(events, "mt295_query_received") != 2 {
		t.Errorf("mt295_query_received count = %d, want 2 (fixture query + this query)", countOp(events, "mt295_query_received"))
	}
}

func TestApplyMT295_UnregisteredMT296Parks(t *testing.T) {
	// A :21: naming no rendered MT296 answer parks, even
	// though nothing about its shape could rule it out — the
	// registry is the whole anchor (LG-478, along the
	// unregistered-MT202 precedent, LG-474).
	l := mt295InboundLedger(t, mustAuditLog(t))
	m, err := ParseMT295(mt295InboundDoc("QRY-295-23", "ANS-296-9999",
		[]string{"STAT"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	res, err := l.ApplyMT295(m, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("ApplyMT295 unregistered MT296 :21:: err = %v, want ErrMT295UnknownReference", err)
	}
	if res.RepairID == "" {
		t.Error("RepairID empty on a parked query, want the repair record")
	}
	// A parked query records nothing: the :20: is not a
	// registered query.
	if _, ok := l.GetMT295Received("QRY-295-23"); ok {
		t.Error("GetMT295Received = true on a parked query, want false (no registration)")
	}
}

func TestApplyMT295_UnregisteredMT296ReapplyStillParks(t *testing.T) {
	// A parked query records nothing, so a re-sent query parks
	// again — under the same :20: or a fresh one — until the
	// queried answer is rendered and registered.
	l := mt295InboundLedger(t, mustAuditLog(t))
	doc := mt295InboundDoc("QRY-295-24", "ANS-296-9999",
		[]string{"STAT"}, nil, nil)
	m, err := ParseMT295(doc)
	if err != nil {
		t.Fatalf("ParseMT295: %v", err)
	}
	first, err := l.ApplyMT295(m, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("first apply: err = %v, want ErrMT295UnknownReference", err)
	}
	if first.RepairID == "" {
		t.Error("first RepairID empty, want the repair record")
	}
	again, err := l.ApplyMT295(m, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("re-apply same :20:: err = %v, want ErrMT295UnknownReference", err)
	}
	if again.RepairID == "" {
		t.Error("re-apply RepairID empty, want the repair record")
	}
	fresh, err := ParseMT295(mt295InboundDoc("QRY-295-24B", "ANS-296-9999",
		[]string{"STAT"}, nil, nil))
	if err != nil {
		t.Fatalf("ParseMT295 fresh doc: %v", err)
	}
	res, err := l.ApplyMT295(fresh, "ops")
	if !errors.Is(err, ErrMT295UnknownReference) {
		t.Fatalf("fresh :20: apply: err = %v, want ErrMT295UnknownReference", err)
	}
	if res.RepairID == "" {
		t.Error("fresh :20: RepairID empty, want the repair record")
	}
}

func TestApplyMT295_OutboundMT296ReplayAndConflict(t *testing.T) {
	// De-dup and conflict semantics along LG-466, on the
	// outbound-MT296 tier: a replay with the same :20: and
	// terms is a no-op with Duplicate set and no second audit
	// marker; a :20: reused for different terms is refused and
	// never overwrites the first registration.
	l, al, mt292Ref, _ := mt296ExportLedger(t)
	if _, err := l.ExportMT296ForAnswered(mt292Ref, mt296ExportOptions(t), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT296ForAnswered: %v", err)
	}
	doc := mt295InboundDoc("QRY-295-25", "ANS-296-0001",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, nil)
	first := applyInboundMT295(t, l, doc, "ops")
	if first.AnchoredKind != MT295AnchorOutboundMT296 {
		t.Fatalf("anchored_kind = %q, want outbound-mt296", first.AnchoredKind)
	}
	dup := applyInboundMT295(t, l, doc, "ops")
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if dup.AnchoredKind != MT295AnchorOutboundMT296 {
		t.Errorf("replay anchored_kind = %q, want outbound-mt296", dup.AnchoredKind)
	}
	events := readAuditOps(t, al)
	// Two markers: the fixture's own query (QRY-296-295) and
	// the first apply — the replay adds none.
	if countOp(events, "mt295_query_received") != 2 {
		t.Errorf("mt295_query_received count = %d, want 2 (no second marker)", countOp(events, "mt295_query_received"))
	}
	conflictDoc := mt295InboundDoc("QRY-295-25", "ANS-296-0001",
		[]string{"DUPL"}, []string{"OPERATOR NOTE"}, nil)
	m, err := ParseMT295(conflictDoc)
	if err != nil {
		t.Fatalf("ParseMT295 conflict doc: %v", err)
	}
	if _, err := l.ApplyMT295(m, "ops"); !errors.Is(err, ErrMT295MsgIDConflict) {
		t.Errorf("ApplyMT295 different terms: err = %v, want ErrMT295MsgIDConflict", err)
	}
	// The first registration is untouched by the refused
	// conflict.
	stored, ok := l.GetMT295Received("QRY-295-25")
	if !ok {
		t.Fatal("GetMT295Received = false, want the first registration")
	}
	if len(stored.Queries) != 1 || stored.Queries[0] != "STAT" || stored.AnchoredKind != MT295AnchorOutboundMT296 {
		t.Errorf("stored = %+v, want the first registration verbatim", stored)
	}
}

func TestApplyMT295_InboundTierBeatsOutboundTier(t *testing.T) {
	// Tier order is deterministic (LG-474): the same :21: hits
	// both the inbound MT202 tier and the outbound MT292 tier —
	// an inbound MT202 :20: takes the outbound MT292 :20: shape
	// — and the inbound tier wins, kind mt202.
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	// The shared :20:: the outbound MT292 reference the paused
	// schedule's export will derive (mt292ExpectedRef, from the
	// schedule's own RunSeq — a fresh fixture schedule's first
	// run is run 0, not run 1).
	sharedRef := mt292ExpectedRef(sched.ID, sched.RunSeq)
	// The inbound MT202: a transfer whose caller-supplied :20:
	// takes the outbound MT292 shape.
	if _, _, err := l.Post(JournalEntry{ID: "fund-nostro-295", DebitAccount: "nostro-acct", CreditAccount: "bank", AmountCents: 1000000, Currency: "USD"}); err != nil {
		t.Fatalf("fund Post: %v", err)
	}
	if _, err := l.RegisterNostroAccount("nostro-acct", "DEUTDEFF", NostroKindNostro); err != nil {
		t.Fatalf("RegisterNostroAccount nostro: %v", err)
	}
	if _, err := l.RegisterNostroAccount("vostro-acct", "BNPAFRPP", NostroKindVostro); err != nil {
		t.Fatalf("RegisterNostroAccount vostro: %v", err)
	}
	if res, err := l.ApplyMT202(mt202ApplyMsg(sharedRef), ""); err != nil || res.Err != nil {
		t.Fatalf("ApplyMT202: err=%v res.Err=%v", err, res.Err)
	}
	// The outbound MT292: the same :20:, derived for the
	// schedule's first run.
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	exp, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	if exp.Reference != sharedRef {
		t.Fatalf("export reference = %q, want %q (the shared :20:)", exp.Reference, sharedRef)
	}
	res := applyInboundMT295(t, l, mt295InboundDoc("QRY-295-20", sharedRef,
		[]string{"STAT"}, nil, nil), "ops")
	if res.AnchoredKind != MT295AnchorMT202 {
		t.Errorf("anchored_kind = %q, want mt202 (inbound tier beats outbound tier)", res.AnchoredKind)
	}
}

func TestApplyMT295_OutboundTierReplayAndConflict(t *testing.T) {
	// De-dup and conflict semantics along LG-466, on an
	// outbound-tier query: a replay with the same :20: and
	// terms is a no-op with Duplicate set and no second audit
	// marker; a :20: reused for different terms is refused and
	// never overwrites the first registration.
	al := mustAuditLog(t)
	l := mt202ExportTestLedger(t, al)
	if _, err := l.ExportMT202Registered(mt202Sample(), "ledger-operator"); err != nil {
		t.Fatalf("ExportMT202Registered: %v", err)
	}
	doc := mt295InboundDoc("QRY-295-21", "MT202-REF-1",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, nil)
	first := applyInboundMT295(t, l, doc, "ops")
	if first.AnchoredKind != MT295AnchorOutboundMT202 {
		t.Fatalf("anchored_kind = %q, want outbound-mt202", first.AnchoredKind)
	}
	dup := applyInboundMT295(t, l, doc, "ops")
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if dup.AnchoredKind != MT295AnchorOutboundMT202 {
		t.Errorf("replay anchored_kind = %q, want outbound-mt202", dup.AnchoredKind)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_received") != 1 {
		t.Errorf("mt295_query_received count = %d, want 1 (no second marker)", countOp(events, "mt295_query_received"))
	}
	conflictDoc := mt295InboundDoc("QRY-295-21", "MT202-REF-1",
		[]string{"DUPL"}, []string{"OPERATOR NOTE"}, nil)
	m, err := ParseMT295(conflictDoc)
	if err != nil {
		t.Fatalf("ParseMT295 conflict doc: %v", err)
	}
	if _, err := l.ApplyMT295(m, "ops"); !errors.Is(err, ErrMT295MsgIDConflict) {
		t.Errorf("ApplyMT295 different terms: err = %v, want ErrMT295MsgIDConflict", err)
	}
	// The first registration is untouched by the refused
	// conflict.
	stored, ok := l.GetMT295Received("QRY-295-21")
	if !ok {
		t.Fatal("GetMT295Received = false, want the first registration")
	}
	if len(stored.Queries) != 1 || stored.Queries[0] != "STAT" || stored.AnchoredKind != MT295AnchorOutboundMT202 {
		t.Errorf("stored = %+v, want the first registration verbatim", stored)
	}
}

func TestApplyMT295_OutboundTierGetterReturnsFirstRegistration(t *testing.T) {
	// The getter returns the first registration verbatim — the
	// :75: / :77A: / :79: texts as received and the outbound
	// kind — not a re-derivation from live facts.
	al := mustAuditLog(t)
	l, sched := mt292ExportTestLedger(t, al)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	exp, err := l.ExportMT292Registered(mt292ExportTestOptions(sched.ID), "ledger-operator")
	if err != nil {
		t.Fatalf("ExportMT292Registered: %v", err)
	}
	applyInboundMT295(t, l, mt295InboundDoc("QRY-295-22", exp.Reference,
		[]string{"STAT", "PLEASE ADVISE STATUS"},
		[]string{"OPERATOR NOTE"},
		[]string{"BANK ASKS FOR THE VALUE DATE"}), "ops")
	stored, ok := l.GetMT295Received("QRY-295-22")
	if !ok {
		t.Fatal("GetMT295Received = false, want the stored query")
	}
	if stored.RelatedReference != exp.Reference || stored.AnchoredKind != MT295AnchorOutboundMT292 ||
		len(stored.Queries) != 2 || stored.Queries[0] != "STAT" || stored.Queries[1] != "PLEASE ADVISE STATUS" ||
		len(stored.Narrative) != 1 || stored.Narrative[0] != "OPERATOR NOTE" ||
		len(stored.Description) != 1 || stored.Description[0] != "BANK ASKS FOR THE VALUE DATE" {
		t.Errorf("stored = %+v, want the registered query verbatim with kind outbound-mt292", stored)
	}
}

func TestApplyMT295_ReplayDuplicateAndConflict(t *testing.T) {
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	registerInboundMT210ForQuery(t, l, "NTF-295-215")
	doc := mt295InboundDoc("QRY-295-15", "NTF-295-215",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, nil)
	first := applyInboundMT295(t, l, doc, "ops")
	// Same :20:, same terms: an idempotent no-op returning the
	// stored outcome, with no second audit marker.
	dup := applyInboundMT295(t, l, doc, "ops")
	if !dup.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if dup.AnchoredKind != first.AnchoredKind || len(dup.Queries) != len(first.Queries) {
		t.Errorf("replay = %+v, want the stored outcome", dup)
	}
	events := readAuditOps(t, al)
	if countOp(events, "mt295_query_received") != 1 {
		t.Errorf("mt295_query_received count = %d, want 1 (no second marker)", countOp(events, "mt295_query_received"))
	}
	// Same :20:, different terms: a conflict, never a silent
	// overwrite — a different question, then a different anchor.
	conflictDoc := mt295InboundDoc("QRY-295-15", "NTF-295-215",
		[]string{"DUPL"}, []string{"OPERATOR NOTE"}, nil)
	m, err := ParseMT295(conflictDoc)
	if err != nil {
		t.Fatalf("ParseMT295 conflict doc: %v", err)
	}
	if _, err := l.ApplyMT295(m, "ops"); !errors.Is(err, ErrMT295MsgIDConflict) {
		t.Errorf("ApplyMT295 different terms: err = %v, want ErrMT295MsgIDConflict", err)
	}
	otherAnchor := mt295InboundDoc("QRY-295-15", "NO-SUCH-MSG",
		[]string{"STAT"}, []string{"OPERATOR NOTE"}, nil)
	m, err = ParseMT295(otherAnchor)
	if err != nil {
		t.Fatalf("ParseMT295 other-anchor doc: %v", err)
	}
	if _, err := l.ApplyMT295(m, "ops"); !errors.Is(err, ErrMT295MsgIDConflict) {
		t.Errorf("ApplyMT295 different anchor: err = %v, want ErrMT295MsgIDConflict", err)
	}
}

func TestApplyMT295_GetterCopyIsolation(t *testing.T) {
	l := mt295InboundLedger(t, mustAuditLog(t))
	// Seed the anchor through the MT210 tier on a ledger that
	// already holds the MT210: use the payout ledger's own
	// MT210 registration path.
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	registerInboundMT210ForQuery(t, l, "NTF-295-216")
	applyInboundMT295(t, l, mt295InboundDoc("QRY-295-16", "NTF-295-216",
		[]string{"STAT", "PLEASE ADVISE"}, []string{"NOTE"}, []string{"DESC"}), "ops")
	stored, ok := l.GetMT295Received("QRY-295-16")
	if !ok {
		t.Fatal("GetMT295Received = false, want the stored query")
	}
	// Mutating the returned slices must not touch the registry.
	stored.Queries[0] = "MUTATED"
	stored.Narrative[0] = "MUTATED"
	stored.Description[0] = "MUTATED"
	again, ok := l.GetMT295Received("QRY-295-16")
	if !ok {
		t.Fatal("GetMT295Received second read = false")
	}
	if again.Queries[0] != "STAT" || again.Narrative[0] != "NOTE" || again.Description[0] != "DESC" {
		t.Errorf("getter copy = %v/%v/%v, want STAT/NOTE/DESC (copy isolation)", again.Queries, again.Narrative, again.Description)
	}
}
