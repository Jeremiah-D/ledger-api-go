package ledger

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// mt210InboundDoc renders a minimal inbound MT210 FIN text: the
// counterparty's notice with :20: msgID, :25: account (an IBAN or
// an account ID), :30: valueDate (6!n YYMMDD), :21:
// notificationID, :32B: amount ("USD100,00"), and debtorLine (the
// full :52A: or :50D: line).
func mt210InboundDoc(msgID, account, valueDate, notificationID, amount, debtorLine string) string {
	return "{1:F01NWBKGB2LXXX0000000000}\n" +
		"{2:I210CITIUS33XXXU3003}\n" +
		"{3:{108:" + msgID + "}}\n" +
		"{4:\n" +
		":20:" + msgID + "\n" +
		":25:" + account + "\n" +
		":30:" + valueDate + "\n" +
		":21:" + notificationID + "\n" +
		":32B:" + amount + "\n" +
		debtorLine + "\n" +
		"-}"
}

// mt210InboundFixture builds a ledger with an audit log and a
// registered IBAN for bob (GB29NWBK60161331926819), the account the
// inbound notices credit.
func mt210InboundFixture(t *testing.T) (*Ledger, *AuditLog) {
	t.Helper()
	al := mustAuditLog(t)
	l := New(WithAuditLog(al))
	if err := l.SetAccountIBAN("bob", "GB29NWBK60161331926819", "NWBKGB2L"); err != nil {
		t.Fatalf("SetAccountIBAN bob: %v", err)
	}
	return l, al
}

// applyInboundMT210 parses and applies one inbound MT210 for the
// fixture, the inbound-apply test convention.
func applyInboundMT210(t *testing.T, l *Ledger, doc, actor string) MT210ApplyResult {
	t.Helper()
	n, err := ParseMT210(doc)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	res, err := l.ApplyMT210(n, actor)
	if err != nil {
		t.Fatalf("ApplyMT210: %v", err)
	}
	return res
}

func TestParseMT210_RoundTrip(t *testing.T) {
	l, sched := mt210TestLedger(t)
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	n, err := ParseMT210(doc)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	if n.MsgID != "sched-1" || n.NotificationID != "sched-1/run/0" {
		t.Errorf("ids = %q/%q, want sched-1/sched-1/run/0", n.MsgID, n.NotificationID)
	}
	if n.AccountIBAN != "GB29NWBK60161331926819" || n.AccountOthr != "" {
		t.Errorf("acct = IBAN %q Othr %q, want IBAN GB29NWBK60161331926819, no Othr", n.AccountIBAN, n.AccountOthr)
	}
	if n.AmountCents != 10000 || n.Currency != "USD" {
		t.Errorf("amount = %d %s, want 10000 USD (fee never deducted)", n.AmountCents, n.Currency)
	}
	if n.ExpectedValueDate != "2026-11-12" {
		t.Errorf("value date = %q, want 2026-11-12", n.ExpectedValueDate)
	}
	if n.Debtor != "DEUTDEFF" || !n.DebtorBIC {
		t.Errorf("debtor = %q BIC %v, want DEUTDEFF true", n.Debtor, n.DebtorBIC)
	}
	if n.SenderLT != "NWBKGB2LXXX" || n.ReceiverLT != "CITIUS33XXX" {
		t.Errorf("LTs = %q/%q, want NWBKGB2LXXX/CITIUS33XXX", n.SenderLT, n.ReceiverLT)
	}
}

func TestParseMT210_RoundTripNoIBANNoBIC(t *testing.T) {
	l := New()
	// "BOBACCT1" is an LT-form account ID with no IBAN
	// registration; "alice" has no registered BIC: the notice
	// falls back to :25: the account ID and :50D: the ordering
	// customer.
	if _, _, err := l.CreateTransferSchedule(TransferSchedule{
		ID: "sched-1", From: "alice", To: "BOBACCT1",
		AmountCents: 10000, Currency: "USD", Interval: 24 * time.Hour,
		NextRunAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateTransferSchedule: %v", err)
	}
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: "sched-1", ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	n, err := ParseMT210(doc)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	if n.AccountIBAN != "" || n.AccountOthr != "BOBACCT1" {
		t.Errorf("acct = IBAN %q Othr %q, want no IBAN, Othr BOBACCT1", n.AccountIBAN, n.AccountOthr)
	}
	if n.Debtor != "ALICE" || n.DebtorBIC {
		t.Errorf("debtor = %q BIC %v, want ALICE false", n.Debtor, n.DebtorBIC)
	}
}

func TestApplyMT210_RegistersReceipt(t *testing.T) {
	l, al := mt210InboundFixture(t)
	doc := mt210InboundDoc("sched-1", "GB29NWBK60161331926819", "261112", "sched-1/run/0", "USD100,00", ":52A:DEUTDEFF")
	versionBefore := l.Version()
	res := applyInboundMT210(t, l, doc, "ops")
	if res.Duplicate {
		t.Error("duplicate = true on first apply, want false")
	}
	r := res.Receipt
	if r.MsgID != "sched-1" || r.NotificationID != "sched-1/run/0" {
		t.Errorf("receipt ids = %q/%q, want sched-1/sched-1/run/0", r.MsgID, r.NotificationID)
	}
	if r.Account != "bob" || r.AmountCents != 10000 || r.Currency != "USD" {
		t.Errorf("receipt = %+v, want account bob 10000 USD", r)
	}
	if r.ExpectedValueDate != "2026-11-12" || r.Debtor != "DEUTDEFF" || r.Actor != "ops" {
		t.Errorf("receipt = %+v, want 2026-11-12/DEUTDEFF/ops", r)
	}
	if r.ReceivedAt.IsZero() {
		t.Error("received_at is zero")
	}
	// A forecast only: no journal rows, no version bump.
	if l.Version() != versionBefore {
		t.Errorf("version = %d, want %d (no bump)", l.Version(), versionBefore)
	}
	if len(l.Entries()) != 0 {
		t.Errorf("entries = %d, want 0", len(l.Entries()))
	}
	// The stored receipt is readable back, and the audit event is
	// precise — and distinct from the camt.057 event.
	got, ok := l.GetExpectedReceipt("sched-1")
	if !ok || got.AmountCents != 10000 || got.Account != "bob" {
		t.Errorf("GetExpectedReceipt = %+v/%v, want bob/10000", got, ok)
	}
	ev := findOp(readAuditOps(t, al), "mt210_notification_received")
	if ev == nil {
		t.Fatal("no mt210_notification_received audit event")
	}
	if ev.Details["msg_id"] != "sched-1" || ev.Details["acct"] != "bob" {
		t.Errorf("audit details = %+v, want msg_id sched-1 acct bob", ev.Details)
	}
	if ev.Details["amount_cents"] != float64(10000) || ev.Details["currency"] != "USD" || ev.Details["expected_value_date"] != "2026-11-12" {
		t.Errorf("audit details = %+v, want 10000/USD/2026-11-12", ev.Details)
	}
	// The same registry feeds intraday liquidity: the notice
	// forecasts the inflow on its expected value date.
	day := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	pos := liquidityPos(t, l.IntradayLiquidity(day), "bob", "USD")
	if pos.NotifiedInflowCents != 10000 {
		t.Errorf("notified inflow = %d, want 10000", pos.NotifiedInflowCents)
	}
}

func TestApplyMT210_ReplayNoop(t *testing.T) {
	l, al := mt210InboundFixture(t)
	doc := mt210InboundDoc("sched-1", "GB29NWBK60161331926819", "261112", "sched-1/run/0", "USD100,00", ":52A:DEUTDEFF")
	first := applyInboundMT210(t, l, doc, "ops")
	second := applyInboundMT210(t, l, doc, "ops")
	if !second.Duplicate {
		t.Error("duplicate = false on replay, want true")
	}
	if !reflect.DeepEqual(second.Receipt, first.Receipt) {
		t.Errorf("replay receipt = %+v, want %+v", second.Receipt, first.Receipt)
	}
	if countOp(readAuditOps(t, al), "mt210_notification_received") != 1 {
		t.Error("replay emitted a second audit event")
	}
}

func TestApplyMT210_Conflict(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	doc := mt210InboundDoc("sched-1", "GB29NWBK60161331926819", "261112", "sched-1/run/0", "USD100,00", ":52A:DEUTDEFF")
	applyInboundMT210(t, l, doc, "ops")
	// The same :20: for a different amount is a conflict.
	other := mt210InboundDoc("sched-1", "GB29NWBK60161331926819", "261112", "sched-1/run/0", "USD200,00", ":52A:DEUTDEFF")
	n, err := ParseMT210(other)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	if _, err := l.ApplyMT210(n, "ops"); !errors.Is(err, ErrMT210MsgIDConflict) {
		t.Errorf("ApplyMT210 amount conflict = %v, want ErrMT210MsgIDConflict", err)
	}
	// The same :20: for a different run is a conflict too.
	otherRun := mt210InboundDoc("sched-1", "GB29NWBK60161331926819", "261112", "sched-1/run/1", "USD100,00", ":52A:DEUTDEFF")
	n2, err := ParseMT210(otherRun)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	if _, err := l.ApplyMT210(n2, "ops"); !errors.Is(err, ErrMT210MsgIDConflict) {
		t.Errorf("ApplyMT210 run conflict = %v, want ErrMT210MsgIDConflict", err)
	}
}

func TestApplyMT210_OthrAccount(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	// alice has journaled entries but no registered IBAN: the
	// :25: account ID names her directly.
	mustLiquidityPost(t, l, JournalEntry{ID: "e-alice-1", DebitAccount: "alice", CreditAccount: "bank", AmountCents: 1000, Currency: "USD", CreatedAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)})
	doc := mt210InboundDoc("othr-1", "alice", "261112", "othr-1/run/0", "USD99,99", ":52A:DEUTDEFF")
	res := applyInboundMT210(t, l, doc, "ops")
	if res.Receipt.Account != "alice" || res.Receipt.AmountCents != 9999 {
		t.Errorf("receipt = %+v, want account alice 9999", res.Receipt)
	}
}

func TestApplyMT210_UnknownAccount(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	// An IBAN the ledger never registered.
	doc := mt210InboundDoc("sched-1", "DE89370400440532013000", "261112", "sched-1/run/0", "USD100,00", ":52A:DEUTDEFF")
	n, err := ParseMT210(doc)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	res, err := l.ApplyMT210(n, "ops")
	if !errors.Is(err, ErrMT210UnknownAccount) {
		t.Fatalf("ApplyMT210 = %v, want ErrMT210UnknownAccount", err)
	}
	if res.RepairID == "" {
		t.Error("repair_id is empty on the parked unknown-account path")
	}
	if _, ok := l.GetExpectedReceipt("sched-1"); ok {
		t.Error("unknown-account notification was registered")
	}
	// An account ID the ledger never heard of, either.
	doc2 := mt210InboundDoc("sched-2", "ghost", "261112", "sched-2/run/0", "USD100,00", ":52A:DEUTDEFF")
	n2, err := ParseMT210(doc2)
	if err != nil {
		t.Fatalf("ParseMT210: %v", err)
	}
	res2, err := l.ApplyMT210(n2, "ops")
	if !errors.Is(err, ErrMT210UnknownAccount) || res2.RepairID == "" {
		t.Errorf("ApplyMT210 ghost = res %+v err %v, want unknown-account + repair_id", res2, err)
	}
}

func TestParseMT210_Rejects(t *testing.T) {
	l, sched := mt210TestLedger(t)
	doc, err := l.ExportMT210(MT210ExportOptions{ScheduleID: sched.ID, ServicingBIC: "CITIUS33"})
	if err != nil {
		t.Fatalf("ExportMT210: %v", err)
	}
	cases := []struct {
		name string
		doc  string
		want error
	}{
		{
			name: "both 50a and 52a present",
			doc:  strings.Replace(doc, "-}", ":50D:ALICE\n-}", 1),
			want: ErrMT210Field,
		},
		{
			name: "missing :32B:",
			doc:  strings.Replace(doc, ":32B:USD100,00\n", "", 1),
			want: ErrMT210Framing,
		},
		{
			name: ":30: eight digits, not 6!n",
			doc:  strings.Replace(doc, ":30:261112", ":30:20261112", 1),
			want: ErrMT210ValueDate,
		},
		{
			name: ":30: non-numeric",
			doc:  strings.Replace(doc, ":30:261112", ":30:26ABCD", 1),
			want: ErrMT210ValueDate,
		},
		{
			name: ":32B: lowercase currency",
			doc:  strings.Replace(doc, ":32B:USD100,00", ":32B:usd100,00", 1),
			want: ErrMT210Currency,
		},
		{
			name: ":32B: zero amount",
			doc:  strings.Replace(doc, ":32B:USD100,00", ":32B:USD0,00", 1),
			want: ErrMT210Amount,
		},
		{
			name: ":50A: option refused",
			doc:  strings.Replace(doc, ":52A:DEUTDEFF", ":50A:DEUTDEFF", 1),
			want: ErrMT210Field,
		},
		{
			name: ":20: over 16x",
			doc:  strings.Replace(doc, ":20:sched-1\n", ":20:sched-1-is-too-long-for-16x\n", 1),
			want: ErrMT210Reference,
		},
		{
			name: "garbage",
			doc:  "not a FIN message",
			want: ErrMT210Framing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMT210(tc.doc); !errors.Is(err, tc.want) {
				t.Errorf("ParseMT210 err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParkMT210Unparsed(t *testing.T) {
	l, _ := mt210InboundFixture(t)
	// A document ParseMT210 refuses but that still names a :20:
	// parks under it.
	doc := mt210InboundDoc("sched-1", "GB29NWBK60161331926819", "261112", "sched-1/run/0", "USD100,00", ":52A:DEUTDEFF")
	broken := strings.Replace(doc, ":32B:USD100,00\n", "", 1)
	if _, err := ParseMT210(broken); err == nil {
		t.Fatal("ParseMT210 accepted the broken document")
	}
	repairID, err := l.ParkMT210Unparsed(broken, "ops")
	if err != nil {
		t.Fatalf("ParkMT210Unparsed: %v", err)
	}
	if repairID == "" {
		t.Error("repair_id is empty for a document naming a :20:")
	}
	// Garbage names nothing: nothing to park.
	if repairID, err := l.ParkMT210Unparsed("not a FIN message", "ops"); err != nil || repairID != "" {
		t.Errorf("ParkMT210Unparsed(garbage) = %q, %v, want \"\", nil", repairID, err)
	}
}
