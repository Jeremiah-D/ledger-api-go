package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// draftKeyedPayout drafts a USD payout with an explicit idempotency
// key — the identity the exported pacs.008 EndToEndId carries.
func draftKeyedPayout(t *testing.T, l *Ledger, id, key string, amount int64) PayoutInstruction {
	t.Helper()
	p, dup, err := l.CreatePayout(PayoutInstruction{
		ID:             id,
		PayeeAccount:   "saver",
		AmountCents:    amount,
		Destination:    "FPS:6222000012345678",
		Maker:          "alice",
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	if dup {
		t.Fatal("duplicate = true on first create")
	}
	return p
}

func TestPacs008Export_HappyPath(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftKeyedPayout(t, l, "po-8e2e-1", "key-8e2e-1", 150_000)

	xmlDoc, err := l.ExportPacs008("po-8e2e-1")
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	body := string(xmlDoc)
	if !strings.Contains(body, `xmlns="urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08"`) {
		t.Error("document does not carry the pacs.008.001.08 namespace")
	}
	if !strings.Contains(body, "<MsgId>po-8e2e-1/pacs008</MsgId>") {
		t.Error("MsgId is not the deterministic <payout>/pacs008")
	}
	if !strings.Contains(body, "<EndToEndId>key-8e2e-1</EndToEndId>") {
		t.Error("EndToEndId does not reuse the payout idempotency key")
	}
	if !strings.Contains(body, "<InstrId>po-8e2e-1</InstrId>") {
		t.Error("InstrId missing")
	}
	if !strings.Contains(body, "<UETR>"+p.UETR+"</UETR>") {
		t.Error("PmtId/UETR does not carry the gpi UETR")
	}
	if !strings.Contains(body, `Ccy="USD"`) {
		t.Error("IntrBkSttlmAmt missing the USD Ccy attribute")
	}
	if !strings.Contains(body, ">1500.00<") {
		t.Error("IntrBkSttlmAmt is not the pain001Amount rendering")
	}
	// Proprietary agent identification (the pacs.009 convention): the
	// payer's account and the operator's destination reference, never
	// a fabricated BIC.
	if !strings.Contains(body, "<Id>saver</Id>") {
		t.Error("payer side does not ride the account ID")
	}
	if !strings.Contains(body, "<Id>FPS:6222000012345678</Id>") {
		t.Error("creditor side does not ride the destination reference")
	}
	if strings.Contains(strings.ToUpper(body), "BIC") {
		t.Error("export must never fabricate BICs")
	}
	if !strings.Contains(body, "<SttlmMtd>INDA</SttlmMtd>") {
		t.Error("SttlmMtd is not INDA")
	}
}

func TestPacs008Export_UETRAssociatesGPITrack(t *testing.T) {
	l := seedPayoutLedger(t)
	p := draftKeyedPayout(t, l, "po-8uetr-1", "key-8uetr-1", 99_00)

	xmlDoc, err := l.ExportPacs008("po-8uetr-1")
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	// The exported UETR resolves through l.gpiByPayout to the track
	// CreatePayout opened: same value, same payout.
	track, ok := l.GetGPITrack(p.UETR)
	if !ok {
		t.Fatalf("no gpi track for UETR %q", p.UETR)
	}
	if track.PayoutID != "po-8uetr-1" {
		t.Errorf("track.PayoutID = %q, want po-8uetr-1", track.PayoutID)
	}
	if !strings.Contains(string(xmlDoc), "<UETR>"+p.UETR+"</UETR>") {
		t.Error("exported document does not carry the gpi track's UETR")
	}
}

func TestPacs008Export_RoundTrip(t *testing.T) {
	l := seedPayoutLedger(t)
	draftKeyedPayout(t, l, "po-8rt-1", "key-8rt-1", 42_500)

	xmlDoc, err := l.ExportPacs008("po-8rt-1")
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	// The outbound document must parse as valid pacs.008 through the
	// inbound parser (the outbound/inbound middle ground).
	txs, err := ParsePacs008(xmlDoc)
	if err != nil {
		t.Fatalf("ParsePacs008(export): %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("parsed %d transactions, want 1", len(txs))
	}
	tx := txs[0]
	if tx.MsgID != "po-8rt-1/pacs008" {
		t.Errorf("MsgID = %q, want po-8rt-1/pacs008", tx.MsgID)
	}
	if tx.EndToEndID != "key-8rt-1" {
		t.Errorf("EndToEndID = %q, want key-8rt-1", tx.EndToEndID)
	}
	if tx.TxID != "po-8rt-1" {
		t.Errorf("TxID = %q, want po-8rt-1", tx.TxID)
	}
	if tx.AmountCents != 42_500 {
		t.Errorf("AmountCents = %d, want 42500", tx.AmountCents)
	}
	if tx.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", tx.Currency)
	}
	if tx.CreditorAccount != "FPS:6222000012345678" {
		t.Errorf("CreditorAccount = %q, want the destination reference", tx.CreditorAccount)
	}
}

func TestPacs008Export_E2EFallsBackToPayoutID(t *testing.T) {
	l := seedPayoutLedger(t)
	// draftPayout sets no idempotency key: the EndToEndId must fall
	// back to the payout ID (the association resolves both), never
	// emit empty.
	draftPayout(t, l, "po-8nokey-1", 10_000)
	xmlDoc, err := l.ExportPacs008("po-8nokey-1")
	if err != nil {
		t.Fatalf("ExportPacs008: %v", err)
	}
	if !strings.Contains(string(xmlDoc), "<EndToEndId>po-8nokey-1</EndToEndId>") {
		t.Error("EndToEndId did not fall back to the payout ID")
	}
	if _, err := ParsePacs008(xmlDoc); err != nil {
		t.Errorf("fallback export does not round-trip: %v", err)
	}
}

func TestPacs008Export_MsgIdDeterministic(t *testing.T) {
	l := seedPayoutLedger(t)
	draftPayout(t, l, "po-8det-1", 10_000)
	first, err := l.ExportPacs008("po-8det-1")
	if err != nil {
		t.Fatalf("first ExportPacs008: %v", err)
	}
	second, err := l.ExportPacs008("po-8det-1")
	if err != nil {
		t.Fatalf("second ExportPacs008: %v", err)
	}
	for _, xmlDoc := range [][]byte{first, second} {
		if !strings.Contains(string(xmlDoc), "<MsgId>po-8det-1/pacs008</MsgId>") {
			t.Error("repeat export does not carry the same deterministic MsgId")
		}
	}
}

func TestPacs008Export_UnknownPayout(t *testing.T) {
	l := seedPayoutLedger(t)
	before := l.Pacs008ExportCount()
	_, err := l.ExportPacs008("po-8nope-1")
	if !errors.Is(err, ErrPayoutNotFound) {
		t.Errorf("err = %v, want ErrPayoutNotFound", err)
	}
	if got := l.Pacs008ExportCount(); got != before {
		t.Errorf("failed export bumped the counter: %d -> %d", before, got)
	}
}

func TestPacs008Export_NonUSD(t *testing.T) {
	l := seedPayoutLedger(t)
	_, dup, err := l.CreatePayout(PayoutInstruction{
		ID:           "po-8eur-1",
		PayeeAccount: "saver",
		AmountCents:  10_000,
		Currency:     "EUR",
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
	})
	if err != nil || dup {
		t.Fatalf("CreatePayout: %v dup=%v", err, dup)
	}
	before := l.Pacs008ExportCount()
	_, err = l.ExportPacs008("po-8eur-1")
	if !errors.Is(err, ErrPacs008ExportNonUSD) {
		t.Errorf("err = %v, want ErrPacs008ExportNonUSD", err)
	}
	if got := l.Pacs008ExportCount(); got != before {
		t.Errorf("refused export bumped the counter: %d -> %d", before, got)
	}
}

func TestPacs008Export_NonPositiveAmount(t *testing.T) {
	l := seedPayoutLedger(t)
	// CreatePayout refuses non-positive amounts, so the payout is
	// injected directly with its gpi track: the export must still be
	// fail-closed.
	uetr := "12345678-1234-4234-8234-123456789abc"
	l.mu.Lock()
	l.payouts["po-8zero-1"] = PayoutInstruction{
		ID:           "po-8zero-1",
		PayeeAccount: "saver",
		AmountCents:  0,
		Currency:     "USD",
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
		UETR:         uetr,
	}
	l.registerGPITrackLocked(uetr, "po-8zero-1", time.Now())
	l.mu.Unlock()

	_, err := l.ExportPacs008("po-8zero-1")
	if !errors.Is(err, ErrPacs008ExportNonPositiveAmount) {
		t.Errorf("err = %v, want ErrPacs008ExportNonPositiveAmount", err)
	}
}

func TestPacs008Export_MissingGPITrack(t *testing.T) {
	l := seedPayoutLedger(t)
	// A payout whose gpi track was never opened cannot be rendered:
	// without the UETR the instruction cannot be tracked end-to-end.
	l.mu.Lock()
	l.payouts["po-8notrack-1"] = PayoutInstruction{
		ID:           "po-8notrack-1",
		PayeeAccount: "saver",
		AmountCents:  10_000,
		Currency:     "USD",
		Destination:  "FPS:6222000012345678",
		Maker:        "alice",
	}
	l.mu.Unlock()

	_, err := l.ExportPacs008("po-8notrack-1")
	if !errors.Is(err, ErrPacs008ExportNoGPITrack) {
		t.Errorf("err = %v, want ErrPacs008ExportNoGPITrack", err)
	}
}

func TestPacs008Export_Counter(t *testing.T) {
	l := seedPayoutLedger(t)
	if got := l.Pacs008ExportCount(); got != 0 {
		t.Fatalf("fresh ledger counter = %d, want 0", got)
	}
	draftPayout(t, l, "po-8cnt-1", 10_000)
	draftPayout(t, l, "po-8cnt-2", 20_000)
	for _, id := range []string{"po-8cnt-1", "po-8cnt-2", "po-8cnt-1"} {
		if _, err := l.ExportPacs008(id); err != nil {
			t.Fatalf("ExportPacs008(%s): %v", id, err)
		}
	}
	if got := l.Pacs008ExportCount(); got != 3 {
		t.Errorf("counter = %d, want 3 (one per successful export)", got)
	}
}
