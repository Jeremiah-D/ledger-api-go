package ledger

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CSV bulk payment file import (LG-208, fintech: enterprise bulk
// payments).
//
// A corporate payment file is a CSV of payment instructions — one row
// per payment: payer, payee, amount, currency, purpose, execution
// date, plus the optional IBAN/BIC columns pain.001 already knows
// how to validate. Importing it is a file *channel*, orthogonal to
// the two neighbouring bulk mechanisms it is most often confused
// with:
//
//   - LG-160 (PostBatchTransfer) is the programmatic batch API: the
//     caller already holds validated Transfer structs. This module
//     is the file channel: the caller holds bytes, and validation
//     happens here, row by row.
//   - OFX import (see ofx_import.go) is a bank-statement channel: it
//     reports what a statement claims against what is already
//     journaled. This module is a payment-instruction channel: it
//     stages payments that do not exist yet.
//
// The import posts nothing. Valid rows stage into a pending-review
// batch; the operator approves the batch with ApproveCSVBatch, which
// posts it atomically through PostBatchTransfer (LG-160) — so every
// posting-time control (denylist, frozen account, overdraft, daily
// limit, period) still applies at approval, and a rejected approval
// posts nothing. A transfer that individually hits its payer's
// large-transfer review threshold journals with the pending-review
// marker (LG-42) exactly as PostTransfer would, and then follows the
// LG-149 batch-review path: CSV batch approval posts the batch, it
// does not waive per-transfer review.
//
// Row validation is fail-closed per row, never per file: a bad row
// parks in the repair queue (LG-177, operator visibility only — the
// pacs.004 precedent: Maker is empty, so the parked record can never
// resubmit as an instruction on its own) and the remaining rows
// still stage. The only whole-file failures are an unreadable file:
// empty input, no header, a missing required column, or a duplicate
// header name.
//
// IBAN/BIC validation reuses the pain.001 validators verbatim
// (validIBAN over the normalized form, validBIC) — no second copy
// of the mod-97 or ISO 9362 rules. Amounts are decimal strings with
// at most two fraction digits, parsed to integer cents without
// floating point, so "100.10" is exactly 10010 cents and "100.105"
// is a bad row, not a rounding decision. Currency is a strict
// 3-letter uppercase code; the execution date is a real calendar
// date in YYYY-MM-DD form.
//
// Duplicate files are idempotent by SHA-256 of the file bytes (the
// OFX precedent): re-importing the same bytes returns the stored
// report with Duplicate set, parks nothing twice, and stages
// nothing twice. A batch's approval is idempotent the same way:
// the batch ID doubles as the PostBatchTransfer idempotency key, so
// a second approval returns the original receipt with Duplicate
// set.
//
// Storage follows the repair-queue precedent, not the OFX one:
// batches are operational pre-posting state in a per-ledger
// registry, not disaster-recovery snapshot state. Re-importing the
// source file after a restore re-stages the same batch ID, so
// nothing is lost by the registry not surviving a snapshot —
// only the approval decision would need re-making, and a batch
// that was already approved replays as a duplicate through the
// batch idempotency key rather than double-posting.

// CSV import errors.
var (
	// ErrInvalidCSVImport is returned by ImportPaymentCSV when the
	// file as a whole is unreadable: empty input, no header row, a
	// missing required column, or a duplicate header name. Per-row
	// problems never return this error — they park in the repair
	// queue and appear in the report.
	ErrInvalidCSVImport = errors.New("ledger: invalid CSV payment file")
	// ErrCSVBatchNotFound is returned by ApproveCSVBatch and
	// GetCSVBatch for an unknown batch ID.
	ErrCSVBatchNotFound = errors.New("ledger: CSV payment batch not found")
)

// CSV batch statuses.
const (
	// CSVBatchStatusPendingReview marks a staged batch awaiting
	// operator approval: nothing has posted.
	CSVBatchStatusPendingReview = "pending_review"
	// CSVBatchStatusApproved marks a batch whose approval posted
	// (or replayed) through PostBatchTransfer.
	CSVBatchStatusApproved = "approved"
)

// csvRequiredColumns are the columns a payment file must carry.
// Optional columns (payer_iban, payee_iban, payer_bic, payee_bic)
// are validated when present and ignored when absent.
var csvRequiredColumns = []string{
	"payer", "payee", "amount", "currency", "purpose", "execution_date",
}

var csvOptionalColumns = []string{
	"payer_iban", "payee_iban", "payer_bic", "payee_bic",
}

// CSVPaymentRow is one validated payment row staged in a batch.
// RowNumber is the file line number (header is line 1), so a repair
// record or an operator can find the row in the source file.
type CSVPaymentRow struct {
	RowNumber     int       `json:"row_number"`
	Payer         AccountID `json:"payer"`
	Payee         AccountID `json:"payee"`
	AmountCents   int64     `json:"amount_cents"`
	Currency      string    `json:"currency"`
	Purpose       string    `json:"purpose"`
	ExecutionDate time.Time `json:"execution_date"`
	PayerIBAN     string    `json:"payer_iban,omitempty"`
	PayeeIBAN     string    `json:"payee_iban,omitempty"`
	PayerBIC      string    `json:"payer_bic,omitempty"`
	PayeeBIC      string    `json:"payee_bic,omitempty"`
}

// CSVBadRow is one row that failed validation: the reason uses the
// repair queue's admitted vocabulary (missing_field /
// invalid_format) so the parked record needs no translation, and
// RepairID names the parked record.
type CSVBadRow struct {
	RowNumber int    `json:"row_number"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail"`
	RepairID  string `json:"repair_id,omitempty"`
}

// CSVPaymentBatch is one staged import: the valid rows awaiting
// approval, plus the batch's identity. Bad rows are not rows here
// — they live in the repair queue and are listed on the report.
type CSVPaymentBatch struct {
	BatchID    string                `json:"batch_id"`
	FileSHA256 string                `json:"file_sha256"`
	Rows       []CSVPaymentRow       `json:"rows"`
	Status     string                `json:"status"`
	ApprovedAt time.Time             `json:"approved_at,omitempty"`
	Receipt    *BatchTransferReceipt `json:"receipt,omitempty"`
}

// CSVImportReport is the outcome of one ImportPaymentCSV call.
// Duplicate marks a re-import of already-seen file bytes: the
// report is the stored one, and nothing was staged or parked again.
type CSVImportReport struct {
	BatchID    string      `json:"batch_id"`
	FileSHA256 string      `json:"file_sha256"`
	TotalRows  int         `json:"total_rows"`
	ValidRows  int         `json:"valid_rows"`
	BadRows    []CSVBadRow `json:"bad_rows"`
	Status     string      `json:"status"`
	Duplicate  bool        `json:"duplicate"`
}

// csvBatchRecord is the registry entry for one batch: the batch
// itself plus its import report (bad rows included, so a re-import
// returns the same report verbatim).
type csvBatchRecord struct {
	batch  CSVPaymentBatch
	report CSVImportReport
}

// csvImportBatches is the per-ledger batch registry. Lock order is
// l.mu first, then this mutex — never the reverse (the repair
// queue's rule). Callers that also call EnqueueRepair or
// PostBatchTransfer must not hold either lock across those calls:
// both take l.mu themselves.
var csvImportBatches = struct {
	sync.Mutex
	m map[*Ledger]map[string]*csvBatchRecord
}{m: make(map[*Ledger]map[string]*csvBatchRecord)}

// csvBatchForLocked returns l's batch registry, creating it on
// first use. Callers must hold l.mu and csvImportBatches.Mutex.
func csvBatchForLocked(l *Ledger) map[string]*csvBatchRecord {
	recs := csvImportBatches.m[l]
	if recs == nil {
		recs = make(map[string]*csvBatchRecord)
		csvImportBatches.m[l] = recs
	}
	return recs
}

// parseCSVAmount parses a decimal amount string to integer cents
// without floating point. At most two fraction digits are allowed;
// more is a precision loss, not a rounding, so it is rejected.
func parseCSVAmount(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("amount is empty")
	}
	intPart, fracPart, hasFrac := s, "", false
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart, hasFrac = s[:i], s[i+1:], true
		if fracPart == "" || len(fracPart) > 2 {
			return 0, fmt.Errorf("amount %q has an invalid fraction", s)
		}
	}
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("amount %q is not numeric", s)
		}
	}
	for _, c := range fracPart {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("amount %q is not numeric", s)
		}
	}
	if intPart == "" {
		return 0, fmt.Errorf("amount %q has no integer part", s)
	}
	intVal, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q overflows int64", s)
	}
	if intVal > (1<<62)/100 {
		return 0, fmt.Errorf("amount %q overflows int64", s)
	}
	cents := intVal * 100
	if hasFrac {
		frac := fracPart
		if len(frac) == 1 {
			frac += "0"
		}
		fracVal, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("amount %q is not numeric", s)
		}
		cents += fracVal
	}
	if cents <= 0 {
		return 0, fmt.Errorf("amount %q is not positive", s)
	}
	if cents > (1 << 62) {
		return 0, fmt.Errorf("amount %q overflows", s)
	}
	return cents, nil
}

// csvField returns the trimmed value of column name in a row, or ""
// when the column is absent from the header.
func csvField(row []string, cols map[string]int, name string) string {
	i, ok := cols[name]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// validateCSVRow validates one data row. The first failure wins:
// a missing required field is missing_field, everything else is
// invalid_format — matching the repair queue's vocabulary, so the
// caller parks the row without translating the reason.
func validateCSVRow(row []string, cols map[string]int, line int) (CSVPaymentRow, string, string) {
	var out CSVPaymentRow
	out.RowNumber = line
	missing := func(field string) (CSVPaymentRow, string, string) {
		return out, RepairReasonMissingField, field + " is required"
	}
	invalid := func(detail string) (CSVPaymentRow, string, string) {
		return out, RepairReasonInvalidFormat, detail
	}
	payer := csvField(row, cols, "payer")
	payee := csvField(row, cols, "payee")
	amount := csvField(row, cols, "amount")
	currency := csvField(row, cols, "currency")
	purpose := csvField(row, cols, "purpose")
	execDate := csvField(row, cols, "execution_date")
	switch {
	case payer == "":
		return missing("payer")
	case payee == "":
		return missing("payee")
	case amount == "":
		return missing("amount")
	case currency == "":
		return missing("currency")
	case purpose == "":
		return missing("purpose")
	case execDate == "":
		return missing("execution_date")
	}
	cents, err := parseCSVAmount(amount)
	if err != nil {
		return invalid(err.Error())
	}
	if len(currency) != 3 {
		return invalid("currency must be a 3-letter code")
	}
	for i := 0; i < 3; i++ {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return invalid("currency must be uppercase A-Z")
		}
	}
	if len(purpose) > 255 {
		return invalid("purpose exceeds the 255-character memo limit")
	}
	parsedDate, err := time.Parse("2006-01-02", execDate)
	if err != nil {
		return invalid("execution_date must be a YYYY-MM-DD calendar date")
	}
	payerIBAN := csvField(row, cols, "payer_iban")
	payeeIBAN := csvField(row, cols, "payee_iban")
	payerBIC := csvField(row, cols, "payer_bic")
	payeeBIC := csvField(row, cols, "payee_bic")
	if payerIBAN != "" && !validIBAN(normalizeIBAN(payerIBAN)) {
		return invalid("payer_iban fails ISO 13616 validation")
	}
	if payeeIBAN != "" && !validIBAN(normalizeIBAN(payeeIBAN)) {
		return invalid("payee_iban fails ISO 13616 validation")
	}
	if payerBIC != "" && !validBIC(payerBIC) {
		return invalid("payer_bic fails ISO 9362 validation")
	}
	if payeeBIC != "" && !validBIC(payeeBIC) {
		return invalid("payee_bic fails ISO 9362 validation")
	}
	out.Payer = AccountID(payer)
	out.Payee = AccountID(payee)
	out.AmountCents = cents
	out.Currency = currency
	out.Purpose = purpose
	out.ExecutionDate = parsedDate
	out.PayerIBAN = payerIBAN
	out.PayeeIBAN = payeeIBAN
	out.PayerBIC = payerBIC
	out.PayeeBIC = payeeBIC
	return out, "", ""
}

// parsePaymentCSV parses file bytes into validated rows and bad
// rows. It holds no ledger state, so the caller can parse before
// taking any lock. Whole-file failures return ErrInvalidCSVImport;
// per-row failures are returned, never errored.
func parsePaymentCSV(data []byte) ([]CSVPaymentRow, []CSVBadRow, error) {
	if len(data) == 0 {
		return nil, nil, ErrInvalidCSVImport
	}
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidCSVImport, err)
	}
	if len(records) == 0 {
		return nil, nil, ErrInvalidCSVImport
	}
	cols := make(map[string]int, len(records[0]))
	for i, name := range records[0] {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, dup := cols[name]; dup {
			return nil, nil, fmt.Errorf("%w: duplicate header %q", ErrInvalidCSVImport, name)
		}
		cols[name] = i
	}
	for _, req := range csvRequiredColumns {
		if _, ok := cols[req]; !ok {
			return nil, nil, fmt.Errorf("%w: missing required column %q", ErrInvalidCSVImport, req)
		}
	}
	// Unknown columns are ignored: a file may carry extra
	// bookkeeping columns without breaking the import.
	var valid []CSVPaymentRow
	var bad []CSVBadRow
	for i, row := range records[1:] {
		line := i + 2 // header is line 1
		if len(row) != len(records[0]) {
			bad = append(bad, CSVBadRow{
				RowNumber: line,
				Reason:    RepairReasonInvalidFormat,
				Detail:    "field count does not match the header",
			})
			continue
		}
		parsed, reason, detail := validateCSVRow(row, cols, line)
		if reason != "" {
			bad = append(bad, CSVBadRow{RowNumber: line, Reason: reason, Detail: detail})
			continue
		}
		valid = append(valid, parsed)
	}
	if valid == nil {
		valid = []CSVPaymentRow{}
	}
	if bad == nil {
		bad = []CSVBadRow{}
	}
	return valid, bad, nil
}

// ImportPaymentCSV imports one bulk payment file: valid rows stage
// into a pending-review batch, bad rows park in the repair queue,
// and nothing posts. Re-importing identical bytes returns the
// stored report with Duplicate set. An empty actor is recorded as
// "csv-import".
func (l *Ledger) ImportPaymentCSV(data []byte, actor string) (*CSVImportReport, error) {
	if actor == "" {
		actor = "csv-import"
	}
	valid, bad, err := parsePaymentCSV(data)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	key := hex.EncodeToString(sum[:])
	batchID := "csvimport-" + key[:16]

	// Duplicate check before parking anything: a re-import must
	// not park the same bad rows a second time.
	l.mu.RLock()
	csvImportBatches.Lock()
	var prev *csvBatchRecord
	if recs, ok := csvImportBatches.m[l]; ok {
		prev = recs[key]
	}
	csvImportBatches.Unlock()
	l.mu.RUnlock()
	if prev != nil {
		dup := prev.report
		dup.Duplicate = true
		return &dup, nil
	}

	// Park bad rows outside any held lock: EnqueueRepair takes
	// l.mu itself. The parked record is operator visibility only
	// (empty Maker, pacs.004 precedent) — it never resubmits as an
	// instruction. It carries only the row identity (batch ID +
	// line number in Destination): a failed row's own values are
	// untrusted by definition, so no payee or amount is invented
	// for it — the operator reads the row from the source file by
	// line number. A parking failure fails the import loud, before
	// anything is staged, so a retry can park and stage cleanly.
	for i := range bad {
		rec, err := l.EnqueueRepair(PayoutInstruction{
			ID:          fmt.Sprintf("%s/row%d", batchID, bad[i].RowNumber),
			AmountCents: 0,
			Currency:    "",
			Destination: fmt.Sprintf("csvimport:%s:row%d", batchID, bad[i].RowNumber),
			// Maker intentionally empty: see the package comment.
		}, bad[i].Reason, actor)
		if err != nil {
			return nil, fmt.Errorf("ledger: parking CSV row %d failed: %w", bad[i].RowNumber, err)
		}
		bad[i].RepairID = rec.ID
	}

	batch := CSVPaymentBatch{
		BatchID:    batchID,
		FileSHA256: key,
		Rows:       valid,
		Status:     CSVBatchStatusPendingReview,
	}
	report := CSVImportReport{
		BatchID:    batchID,
		FileSHA256: key,
		TotalRows:  len(valid) + len(bad),
		ValidRows:  len(valid),
		BadRows:    bad,
		Status:     CSVBatchStatusPendingReview,
	}
	l.mu.Lock()
	csvImportBatches.Lock()
	recs := csvBatchForLocked(l)
	if existing, ok := recs[key]; ok {
		// A concurrent import won the race: return its report
		// as a duplicate. The rows parked above are extra
		// visibility records for the same bad rows, not a second
		// staging — the batch itself was stored exactly once.
		csvImportBatches.Unlock()
		l.mu.Unlock()
		dup := existing.report
		dup.Duplicate = true
		return &dup, nil
	}
	recs[key] = &csvBatchRecord{batch: batch, report: report}
	csvImportBatches.Unlock()
	l.mu.Unlock()
	out := report
	return &out, nil
}

// GetCSVBatch returns a copy of one staged batch, or false when
// the batch ID is unknown.
func (l *Ledger) GetCSVBatch(batchID string) (CSVPaymentBatch, bool) {
	l.mu.RLock()
	csvImportBatches.Lock()
	var found *csvBatchRecord
	for _, rec := range csvImportBatches.m[l] {
		if rec.batch.BatchID == batchID {
			found = rec
			break
		}
	}
	csvImportBatches.Unlock()
	l.mu.RUnlock()
	if found == nil {
		return CSVPaymentBatch{}, false
	}
	out := found.batch
	out.Rows = append([]CSVPaymentRow(nil), found.batch.Rows...)
	if found.batch.Receipt != nil {
		receipt := *found.batch.Receipt
		out.Receipt = &receipt
	}
	return out, true
}

// ApproveCSVBatch approves one pending-review batch: its valid rows
// post atomically through PostBatchTransfer, in file order, with
// the row's execution date as the transfer date and the row's
// purpose as the transfer memo. A second approval replays the
// original receipt with Duplicate set. A failed approval leaves
// the batch pending_review and posts nothing (PostBatchTransfer
// atomicity), so the operator can fix the cause — a frozen account,
// a denylisted payee — and approve again.
func (l *Ledger) ApproveCSVBatch(batchID, actor string) (BatchTransferReceipt, error) {
	if actor == "" {
		actor = "csv-import"
	}
	batch, ok := l.GetCSVBatch(batchID)
	if !ok {
		return BatchTransferReceipt{}, ErrCSVBatchNotFound
	}
	if batch.Status == CSVBatchStatusApproved && batch.Receipt != nil {
		replay := *batch.Receipt
		replay.Duplicate = true
		return replay, nil
	}
	transfers := make([]Transfer, 0, len(batch.Rows))
	for _, row := range batch.Rows {
		transfers = append(transfers, Transfer{
			ID:          fmt.Sprintf("%s/row%d", batch.BatchID, row.RowNumber),
			From:        row.Payer,
			To:          row.Payee,
			AmountCents: row.AmountCents,
			Currency:    row.Currency,
			Memo:        row.Purpose,
			ExternalRef: fmt.Sprintf("csvimport:%s:row%d", batch.BatchID, row.RowNumber),
			CreatedAt:   row.ExecutionDate,
			Metadata: map[string]string{
				"csv_batch":          batch.BatchID,
				"csv_execution_date": row.ExecutionDate.Format("2006-01-02"),
			},
		})
	}
	receipt, err := l.PostBatchTransfer(TransferBatch{
		ID:             batch.BatchID,
		Transfers:      transfers,
		IdempotencyKey: batch.BatchID,
	})
	if err != nil {
		return BatchTransferReceipt{}, err
	}
	// Mark approved under both locks (l.mu first, registry second).
	l.mu.Lock()
	csvImportBatches.Lock()
	for _, rec := range csvImportBatches.m[l] {
		if rec.batch.BatchID == batchID {
			rec.batch.Status = CSVBatchStatusApproved
			rec.batch.ApprovedAt = time.Now()
			stored := receipt
			rec.batch.Receipt = &stored
			rec.report.Status = CSVBatchStatusApproved
			break
		}
	}
	csvImportBatches.Unlock()
	l.mu.Unlock()
	return receipt, nil
}
