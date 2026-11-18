package ledger

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mt292InboundDoc renders a minimal inbound MT292 FIN text: the
// counterparty's cancellation request with :20: msgID, :21:
// relatedMsgID (the original MT210's :20:), :11S: orig11S
// ("210" plus a YYMMDD date), and :79: reason (an empty reason
// omits the field; embedded newlines split it into continuation
// lines).
func mt292InboundDoc(msgID, relatedMsgID, orig11S, reason string) string {
	var b strings.Builder
	b.WriteString("{1:F01NWBKGB2LXXX0000000000}\n")
	b.WriteString("{2:I292CITIUS33XXXU3003}\n")
	b.WriteString("{3:{108:" + msgID + "}}\n")
	b.WriteString("{4:\n")
	b.WriteString(":20:" + msgID + "\n")
	b.WriteString(":21:" + relatedMsgID + "\n")
	b.WriteString(":11S:" + orig11S + "\n")
	if reason != "" {
		b.WriteString(":79:" + reason + "\n")
	}
	b.WriteString("-}")
	return b.String()
}

// registerInboundMT210ForCancel registers one inbound MT210
// notification on the MT210 inbound fixture, the notification a
// later MT292 request withdraws.
func registerInboundMT210ForCancel(t *testing.T, l *Ledger, msgID, notificationID, valueDate string) ExpectedReceipt {
	t.Helper()
	doc := mt210InboundDoc(msgID, "GB29NWBK60161331926819", valueDate, notificationID, "USD100,00", ":52A:DEUTDEFF")
	return applyInboundMT210(t, l, doc, "ops").Receipt
}

// applyInboundMT292 parses and applies one inbound MT292 for the
// fixture, the inbound-apply test convention.
func applyInboundMT292(t *testing.T, l *Ledger, doc, actor string) MT292ApplyResult {
	t.Helper()
	req, err := ParseMT292(doc)
	if err != nil {
		t.Fatalf("ParseMT292: %v", err)
	}
	res, err := l.ApplyMT292(req, actor)
	if err != nil {
		t.Fatalf("ApplyMT292: %v", err)
	}
	return res
}

func TestParseMT292_RoundTrip(t *testing.T) {
	l, sched := mt292TestLedger(t)
	if _, err := l.PauseTransferSchedule(sched.ID); err != nil {
		t.Fatalf("PauseTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT292(MT292ExportOptions{
		ScheduleID: sched.ID, ServicingBIC: "CITIUS33",
		OriginalDate: mt292OriginalDate(),
		Reason:       "Schedule paused by operator",
	})
	if err != nil {
		t.Fatalf("ExportMT292: %v", err)
	}
	req, err := ParseMT292(doc)
	if err != nil {
		t.Fatalf("ParseMT292: %v", err)
	}
	ref := mt292ExpectedRef(sched.ID, sched.RunSeq)
	if req.MsgID != ref || req.RelatedMsgID != "sched-1" {
		t.Errorf("ids = %q/%q, want %s/sched-1", req.MsgID, req.RelatedMsgID, ref)
	}
	if req.OriginalType != "210" || req.OriginalDate != "261110" {
		t.Errorf(":11S: = %q/%q, want 210/261110", req.OriginalType, req.OriginalDate)
	}
	if req.Reason != "Schedule paused by operator" {
		t.Errorf("reason = %q, want the verbatim reason", req.Reason)
	}
	if req.SenderLT != "NWBKGB2LXXX" || req.ReceiverLT != "CITIUS33XXX" {
		t.Errorf("LTs = %q/%q, want NWBKGB2LXXX/CITIUS33XXX", req.SenderLT, req.ReceiverLT)
	}
	// CRLF endings and a trailing newline after the trailer are
	// tolerated, like the MT210 parse.
	crlf := strings.ReplaceAll(doc, "\n", "\r\n") + "\n"
	if _, err := ParseMT292(crlf); err != nil {
		t.Errorf("ParseMT292 CRLF/trailing-newline: %v", err)
	}
}

func TestParseMT292_MultiLineReason(t *testing.T) {
	doc := mt292InboundDoc("MT292-0123456789", "sched-1", "210261110",
		"Client withdrew the order\nduplicate notice\nplease disregard")
	req, err := ParseMT292(doc)
	if err != nil {
		t.Fatalf("ParseMT292: %v", err)
	}
	if req.Reason != "Client withdrew the order\nduplicate notice\nplease disregard" {
		t.Errorf("reason = %q, want the three lines joined", req.Reason)
	}
	// No :79: at all: the reason reads as "".
	doc = mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", "")
	req, err = ParseMT292(doc)
	if err != nil {
		t.Fatalf("ParseMT292 no-reason: %v", err)
	}
	if req.Reason != "" {
		t.Errorf("reason = %q, want empty", req.Reason)
	}
}

func TestParseMT292Rejects(t *testing.T) {
	tooLong := strings.Repeat("x", 36)
	tooMany := strings.Repeat("line\n", 35) + "line"
	cases := []struct {
		name string
		doc  string
		want error
	}{
		{"empty document", "", ErrInvalidMT292},
		{"wrong block 2 type", strings.Replace(mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", "reason"), "{2:I292", "{2:I210", 1), ErrInvalidMT292},
		{"non-210 original type", mt292InboundDoc("MT292-0123456789", "sched-1", "103261110", "reason"), ErrMT292UnsupportedOriginal},
		{"bad :11S: date", mt292InboundDoc("MT292-0123456789", "sched-1", "210261332", "reason"), ErrInvalidMT292},
		{"short :11S:", mt292InboundDoc("MT292-0123456789", "sched-1", "2102611", "reason"), ErrInvalidMT292},
		{"reordered fields", "{1:F01NWBKGB2LXXX0000000000}\n{2:I292CITIUS33XXXU3003}\n{3:{108:MT292-0123456789}}\n{4:\n:21:sched-1\n:20:MT292-0123456789\n:11S:210261110\n-}", ErrInvalidMT292},
		{"missing :11S:", "{1:F01NWBKGB2LXXX0000000000}\n{2:I292CITIUS33XXXU3003}\n{3:{108:MT292-0123456789}}\n{4:\n:20:MT292-0123456789\n:21:sched-1\n-}", ErrInvalidMT292},
		{"unknown field", "{1:F01NWBKGB2LXXX0000000000}\n{2:I292CITIUS33XXXU3003}\n{3:{108:MT292-0123456789}}\n{4:\n:20:MT292-0123456789\n:21:sched-1\n:11S:210261110\n:25:ACCT\n-}", ErrInvalidMT292},
		{":79: line too long", mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", tooLong), ErrInvalidMT292},
		{":79: too many lines", mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", tooMany), ErrInvalidMT292},
		{"missing trailer", strings.TrimSuffix(mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", "reason"), "-}"), ErrInvalidMT292},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT292(tc.doc); err == nil {
				t.Fatal("ParseMT292: got nil error, want a refusal")
			} else if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestApplyMT292_CancelsRegisteredMT210(t *testing.T) {
	l, al := mt210InboundFixture(t)
	registerInboundMT210ForCancel(t, l, "sched-1", "sched-1/run/0", "261112")
	versionBefore := l.Version()
	doc := mt292InboundDoc("MT292-0123456789", "sched-1", "210261110",
		"Client withdrew the order\nduplicate notice")
	res := applyInboundMT292(t, l, doc, "ops")
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	r := res.Receipt
	if !r.Cancelled || r.CancelMsgID != "MT292-0123456789" {
		t.Errorf("receipt cancelled = %v/%q, want true/MT292-0123456789", r.Cancelled, r.CancelMsgID)
	}
	if r.CancelReason != "Client withdrew the order\nduplicate notice" {
		t.Errorf("cancel reason = %q, want the verbatim reason", r.CancelReason)
	}
	if r.CancelledAt.IsZero() {
		t.Error("cancelled_at is zero")
	}
	// The notification's own fields are preserved, not rewritten.
	if r.MsgID != "sched-1" || r.NotificationID != "sched-1/run/0" {
		t.Errorf("receipt ids = %q/%q, want sched-1/sched-1/run/0", r.MsgID, r.NotificationID)
	}
	if r.Account != "bob" || r.AmountCents != 10000 || r.Currency != "USD" {
		t.Errorf("receipt = %+v, want account bob 10000 USD", r)
	}
	if r.ExpectedValueDate != "2026-11-12" || r.Debtor != "DEUTDEFF" {
		t.Errorf("receipt = %+v, want 2026-11-12/DEUTDEFF", r)
	}
	// Only the forecast changes: no journal rows, no version bump.
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (no bump)", l.Version(), versionBefore)
	}
	if len(l.Entries()) != 0 {
		t.Errorf("entries = %d, want 0", len(l.Entries()))
	}
	// The stored receipt reads back cancelled, and the audit
	// event is precise.
	got, ok := l.GetExpectedReceipt("sched-1")
	if !ok || !got.Cancelled || got.CancelMsgID != "MT292-0123456789" || got.CancelReason != "Client withdrew the order\nduplicate notice" {
		t.Errorf("GetExpectedReceipt = %+v/%v, want cancelled MT292-0123456789 with the verbatim reason", got, ok)
	}
	ev := findOp(readAuditOps(t, al), "mt210_notification_cancelled")
	if ev == nil {
		t.Fatal("no mt210_notification_cancelled audit event")
	}
	if ev.Details["msg_id"] != "MT292-0123456789" || ev.Details["orgnl_msg_id"] != "sched-1" {
		t.Errorf("audit details = %+v, want msg_id MT292-0123456789 orgnl_msg_id sched-1", ev.Details)
	}
	if ev.Details["reason"] != "Client withdrew the order\nduplicate notice" {
		t.Errorf("audit details = %+v, want the verbatim reason", ev.Details)
	}
	if len(ev.Accounts) != 1 || ev.Accounts[0] != "bob" {
		t.Errorf("audit accounts = %v, want [bob]", ev.Accounts)
	}
	if countOp(readAuditOps(t, al), "mt210_notification_cancelled") != 1 {
		t.Error("expected exactly one cancellation audit event")
	}
}

func TestApplyMT292_NoReason(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	registerInboundMT210ForCancel(t, l, "sched-1", "sched-1/run/0", "261112")
	doc := mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", "")
	res := applyInboundMT292(t, l, doc, "ops")
	if res.Receipt.CancelReason != "" {
		t.Errorf("cancel reason = %q, want empty", res.Receipt.CancelReason)
	}
	if !res.Receipt.Cancelled {
		t.Error("receipt is not cancelled")
	}
}

func TestApplyMT292UnknownNotification(t *testing.T) {
	l, al := mt210InboundFixture(t)
	doc := mt292InboundDoc("MT292-0123456789", "sched-never", "210261110", "reason")
	req, err := ParseMT292(doc)
	if err != nil {
		t.Fatalf("ParseMT292: %v", err)
	}
	res, err := l.ApplyMT292(req, "ops")
	if !errors.Is(err, ErrMT292UnknownNotification) {
		t.Fatalf("ApplyMT292 = %v, want ErrMT292UnknownNotification", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty on the parked unknown-original path")
	}
	// The ledger never invents a notification in order to cancel
	// it: nothing is registered under the named original.
	if _, ok := l.GetExpectedReceipt("sched-never"); ok {
		t.Error("unknown original was registered")
	}
	if countOp(readAuditOps(t, al), "mt210_notification_cancelled") != 0 {
		t.Error("unknown-original refusal emitted a cancellation audit event")
	}
}

func TestApplyMT292DuplicateAndConflict(t *testing.T) {
	l, al := mt210InboundFixture(t)
	registerInboundMT210ForCancel(t, l, "sched-1", "sched-1/run/0", "261112")
	doc := mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", "reason")
	first := applyInboundMT292(t, l, doc, "ops")
	second := applyInboundMT292(t, l, doc, "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if !reflect.DeepEqual(second.Receipt, first.Receipt) {
		t.Errorf("replay receipt = %+v, want %+v", second.Receipt, first.Receipt)
	}
	if countOp(readAuditOps(t, al), "mt210_notification_cancelled") != 1 {
		t.Error("replay emitted a second audit event")
	}
	// The same request :20: for a different original is a
	// conflict.
	registerInboundMT210ForCancel(t, l, "sched-2", "sched-2/run/0", "261113")
	other := mt292InboundDoc("MT292-0123456789", "sched-2", "210261110", "reason")
	req, err := ParseMT292(other)
	if err != nil {
		t.Fatalf("ParseMT292: %v", err)
	}
	if _, err := l.ApplyMT292(req, "ops"); !errors.Is(err, ErrMT292MsgIDConflict) {
		t.Errorf("ApplyMT292 conflict = %v, want ErrMT292MsgIDConflict", err)
	}
}

func TestApplyMT292SecondRequestDuplicate(t *testing.T) {
	l, al := mt210InboundFixture(t)
	registerInboundMT210ForCancel(t, l, "sched-1", "sched-1/run/0", "261112")
	firstDoc := mt292InboundDoc("MT292-0123456789", "sched-1", "210261110", "first reason")
	first := applyInboundMT292(t, l, firstDoc, "ops")
	// A different request :20: cancelling an already-cancelled
	// notification: a no-op success returning the stored
	// (cancelled) receipt, with the first request still named as
	// the canceller.
	secondDoc := mt292InboundDoc("MT292-9876543210", "sched-1", "210261110", "second reason")
	second := applyInboundMT292(t, l, secondDoc, "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on second request, want true")
	}
	if !reflect.DeepEqual(second.Receipt, first.Receipt) {
		t.Errorf("second-request receipt = %+v, want %+v", second.Receipt, first.Receipt)
	}
	if second.Receipt.CancelMsgID != "MT292-0123456789" || second.Receipt.CancelReason != "first reason" {
		t.Errorf("receipt = %+v, want canceller MT292-0123456789/first reason", second.Receipt)
	}
	if countOp(readAuditOps(t, al), "mt210_notification_cancelled") != 1 {
		t.Error("second request emitted a second audit event")
	}
	// The second request's own replay converges the same way.
	third := applyInboundMT292(t, l, secondDoc, "ops")
	if !third.Duplicate || !reflect.DeepEqual(third.Receipt, first.Receipt) {
		t.Errorf("second-request replay = %+v, want duplicate of %+v", third.Receipt, first.Receipt)
	}
}

func TestIntradayLiquidityMT292CancelledNotification(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	registerInboundMT210ForCancel(t, l, "msg-liq-a", "msg-liq-a/run/0", "261120")
	registerInboundMT210ForCancel(t, l, "msg-liq-b", "msg-liq-b/run/0", "261120")
	registerInboundMT210ForCancel(t, l, "msg-liq-c", "msg-liq-c/run/0", "261121")
	doc := mt292InboundDoc("MT292-0123456789", "msg-liq-a", "210261110", "withdrawn")
	applyInboundMT292(t, l, doc, "ops")

	day := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	pos := liquidityPos(t, l.IntradayLiquidity(day), "bob", "USD")
	if pos.NotifiedInflowCents != 10000 {
		t.Errorf("notified inflow = %d, want 10000 (cancelled 10000 excluded)", pos.NotifiedInflowCents)
	}
	if pos.NetPositionCents != 10000 {
		t.Errorf("net = %d, want 10000", pos.NetPositionCents)
	}
	// The other day's notification is untouched.
	otherDay := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	other := liquidityPos(t, l.IntradayLiquidity(otherDay), "bob", "USD")
	if other.NotifiedInflowCents != 10000 {
		t.Errorf("other-day notified inflow = %d, want 10000", other.NotifiedInflowCents)
	}
}

func TestParkMT292Unparsed(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	// A document that fails the parse but still names its :21:
	// parks.
	broken := strings.Replace(mt292InboundDoc("MT292-0123456789", "sched-park", "210261110", "reason"), ":11S:210261110\n", "", 1)
	if _, err := ParseMT292(broken); err == nil {
		t.Fatal("ParseMT292 accepted a document without :11S:")
	}
	id, err := l.ParkMT292Unparsed(broken, "ops")
	if err != nil {
		t.Fatalf("ParkMT292Unparsed: %v", err)
	}
	if id == "" {
		t.Error("ParkMT292Unparsed returned an empty repair ID for a document naming :21:")
	}
	// Garbage that names nothing parks nothing.
	id, err = l.ParkMT292Unparsed("not a FIN document", "ops")
	if err != nil {
		t.Fatalf("ParkMT292Unparsed garbage: %v", err)
	}
	if id != "" {
		t.Errorf("ParkMT292Unparsed garbage = %q, want empty", id)
	}
}
