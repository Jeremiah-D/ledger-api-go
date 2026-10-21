package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// NACHA ACH file generation (LG-157, fintech: ACH 出款文件).
//
// From ledger payout instructions (or any caller-supplied entry list)
// this module builds a NACHA-formatted ACH origination file: File
// Header (1), per-batch Batch Header (5) / Entry Detail (6) / optional
// Addenda (7) / Batch Control (8), and the File Control (9). Every
// record is exactly 94 characters; the file is padded with "9" records
// to a 10-record block.
//
// Scope honesty: this is an origination (debit/credit) file builder,
// not a bank. Standard Entry Class is CCD or PPD — the two corporate/
// prearranged classes payouts map to. Same-day/window codes, IAT, and
// return handling are out of scope (documented, not silently faked).
//
// Field discipline is fail-loud: text that does not fit its field or
// is not printable ASCII is an error, never silently truncated — a
// payment file must not lose digits or rename payees quietly.

// Validation and config errors returned by the NACHA builder.
var (
	// ErrNACHARouting is returned when a routing/transit number is not
	// 9 (or 8, where accepted) decimal digits.
	ErrNACHARouting = errors.New("ledger: invalid NACHA routing number")
	// ErrNACHACheckDigit is returned when the 9th digit of a routing
	// number does not match the 3-7-1 checksum of the first 8.
	ErrNACHACheckDigit = errors.New("ledger: NACHA routing check digit mismatch")
	// ErrNACHATransactionCode is returned for a transaction code outside
	// the supported CCD/PPD set.
	ErrNACHATransactionCode = errors.New("ledger: unsupported NACHA transaction code")
	// ErrNACHAServiceClass is returned when an entry's debit/credit
	// direction is forbidden by the batch's service class code (e.g. a
	// debit entry in a 220 credits-only batch).
	ErrNACHAServiceClass = errors.New("ledger: entry direction forbidden by batch service class")
	// ErrNACHAFieldTooLong is returned when a text field exceeds its
	// fixed NACHA width: the builder never truncates payment data.
	ErrNACHAFieldTooLong = errors.New("ledger: NACHA field exceeds its fixed width")
	// ErrNACHANonASCII is returned when a text field carries characters
	// outside printable ASCII (0x20–0x7E): the format is ASCII-only, so
	// the caller must transliterate first.
	ErrNACHANonASCII = errors.New("ledger: NACHA fields must be printable ASCII")
	// ErrNACHAAmount is returned for a negative amount or one that does
	// not fit the 10-digit cents field.
	ErrNACHAAmount = errors.New("ledger: invalid NACHA amount")
	// ErrNACHAConfig is returned for an invalid file or batch config
	// (unknown service class code, SEC code, effective date, …).
	ErrNACHAConfig = errors.New("ledger: invalid NACHA file/batch configuration")
	// ErrNACHAEmpty is returned when asked to build a file with no
	// batches or a batch with no entries: a payment file with no
	// payments is a caller bug.
	ErrNACHAEmpty = errors.New("ledger: NACHA file needs at least one batch with at least one entry")
	// ErrNACHAAddenda is returned when one entry carries more addenda
	// records than the 4-digit addenda sequence number can address.
	ErrNACHAAddenda = errors.New("ledger: too many NACHA addenda records for one entry")
)

// nachaRecordLen is the fixed NACHA record length; nachaBlockLen the
// blocking factor (records per block).
const (
	nachaRecordLen = 94
	nachaBlockLen  = 10
)

// Supported NACHA transaction codes for CCD/PPD origination. Credits
// move money to the receiver (payouts); debits pull from it.
var nachaCreditCodes = map[string]bool{"22": true, "32": true}
var nachaDebitCodes = map[string]bool{"27": true, "37": true}

// nachaPrenoteCodes are zero-dollar pre-notifications: allowed, but the
// amount must be exactly zero.
var nachaPrenoteCodes = map[string]bool{"23": true, "28": true, "33": true, "38": true}

// nachaServiceClasses are the batch service class codes: 200 mixed
// debits and credits, 220 credits only, 225 debits only.
var nachaServiceClasses = map[string]bool{"200": true, "220": true, "225": true}

// nachaSECCodes are the supported Standard Entry Class codes.
var nachaSECCodes = map[string]bool{"CCD": true, "PPD": true}

// nachaCheckWeights is the ABA routing checksum weighting 3-7-1
// repeating over the first 8 digits.
var nachaCheckWeights = [8]int{3, 7, 1, 3, 7, 1, 3, 7}

// nachaCheckDigitOf8 returns the check digit for an 8-digit transit
// number.
func nachaCheckDigitOf8(first8 string) byte {
	sum := 0
	for i := 0; i < 8; i++ {
		sum += int(first8[i]-'0') * nachaCheckWeights[i]
	}
	return byte('0' + (10-(sum%10))%10)
}

// allDigits reports whether s is n ASCII decimal digits.
func nachaAllDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// NACHACheckDigit validates a 9-digit routing number's check digit and
// returns it. It is an error when the number is not 9 digits or the
// checksum fails — the same guard the file builder applies to every
// receiving DFI and to the immediate destination/origin.
func NACHACheckDigit(routing string) (byte, error) {
	if !nachaAllDigits(routing, 9) {
		return 0, fmt.Errorf("%w: %q", ErrNACHARouting, routing)
	}
	if want := nachaCheckDigitOf8(routing[:8]); routing[8] != want {
		return 0, fmt.Errorf("%w: %q (want %c)", ErrNACHACheckDigit, routing, want)
	}
	return routing[8], nil
}

// nachaPrintable reports whether s is entirely printable ASCII.
func nachaPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

// nachaAlpha renders a left-justified blank-filled field. Overlong or
// non-ASCII input is an error — the builder never truncates.
func nachaAlpha(s string, width int) (string, error) {
	if len(s) > width {
		return "", fmt.Errorf("%w: %q exceeds %d", ErrNACHAFieldTooLong, s, width)
	}
	if !nachaPrintable(s) {
		return "", fmt.Errorf("%w: %q", ErrNACHANonASCII, s)
	}
	return s + strings.Repeat(" ", width-len(s)), nil
}

// nachaNumeric renders a right-justified zero-filled numeric field.
func nachaNumeric(v int64, width int) (string, error) {
	if v < 0 {
		return "", fmt.Errorf("%w: negative %d", ErrNACHAAmount, v)
	}
	s := fmt.Sprintf("%d", v)
	if len(s) > width {
		return "", fmt.Errorf("%w: %d exceeds %d digits", ErrNACHAAmount, v, width)
	}
	return strings.Repeat("0", width-len(s)) + s, nil
}

// nachaAmount renders the 10-digit cents amount field.
func nachaAmount(cents int64) (string, error) {
	return nachaNumeric(cents, 10)
}

// nachaYYMMDD renders a date field. Zero time is an error at the call
// sites that require a date; callers that allow "today" resolve it
// before calling.
func nachaYYMMDD(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%02d%02d%02d", t.Year()%100, int(t.Month()), t.Day())
}

// NACHAEntryDirection is the money direction of an entry from the
// originator's view: "credit" pays the receiver (a payout), "debit"
// pulls from them.
type NACHAEntryDirection string

const (
	// NACHAEntryCredit: transaction codes 22/32 — money to the receiver.
	NACHAEntryCredit NACHAEntryDirection = "credit"
	// NACHAEntryDebit: transaction codes 27/37 — money from the receiver.
	NACHAEntryDebit NACHAEntryDirection = "debit"
)

// NACHAEntry is one ACH entry detail: the receiver, the amount, and the
// direction. Routing is the receiver's 9-digit routing number (check
// digit validated); TraceSeq is assigned by the builder when zero.
type NACHAEntry struct {
	TransactionCode string
	Routing         string
	AccountNumber   string
	AmountCents     int64
	IndividualID    string
	IndividualName  string
	Discretionary   string
	// Addenda holds optional payment-related lines (≤80 chars each),
	// emitted as type "05" addenda records after the entry detail.
	Addenda []string
}

// Direction resolves the entry's money direction from its transaction
// code.
func (e NACHAEntry) Direction() (NACHAEntryDirection, error) {
	if nachaCreditCodes[e.TransactionCode] || e.TransactionCode == "23" || e.TransactionCode == "33" {
		return NACHAEntryCredit, nil
	}
	if nachaDebitCodes[e.TransactionCode] || e.TransactionCode == "28" || e.TransactionCode == "38" {
		return NACHAEntryDebit, nil
	}
	return "", fmt.Errorf("%w: %q", ErrNACHATransactionCode, e.TransactionCode)
}

// validate checks the entry's static shape: transaction code known,
// routing valid, fields within width, amount in range, prenotes zero.
func (e NACHAEntry) validate() error {
	if !nachaCreditCodes[e.TransactionCode] && !nachaDebitCodes[e.TransactionCode] && !nachaPrenoteCodes[e.TransactionCode] {
		return fmt.Errorf("%w: %q", ErrNACHATransactionCode, e.TransactionCode)
	}
	if _, err := NACHACheckDigit(e.Routing); err != nil {
		return err
	}
	if len(e.AccountNumber) > 17 {
		return fmt.Errorf("%w: account number exceeds 17", ErrNACHAFieldTooLong)
	}
	if !nachaPrintable(e.AccountNumber) {
		return fmt.Errorf("%w: account number", ErrNACHANonASCII)
	}
	if e.AmountCents < 0 || e.AmountCents > 9999999999 {
		return fmt.Errorf("%w: %d", ErrNACHAAmount, e.AmountCents)
	}
	if nachaPrenoteCodes[e.TransactionCode] && e.AmountCents != 0 {
		return fmt.Errorf("%w: prenote %q must be zero-dollar", ErrNACHATransactionCode, e.TransactionCode)
	}
	if len(e.IndividualID) > 15 {
		return fmt.Errorf("%w: individual ID exceeds 15", ErrNACHAFieldTooLong)
	}
	if len(e.IndividualName) > 22 {
		return fmt.Errorf("%w: individual name exceeds 22", ErrNACHAFieldTooLong)
	}
	if len(e.Discretionary) > 2 {
		return fmt.Errorf("%w: discretionary exceeds 2", ErrNACHAFieldTooLong)
	}
	for _, f := range []string{e.IndividualID, e.IndividualName, e.Discretionary} {
		if !nachaPrintable(f) {
			return fmt.Errorf("%w: %q", ErrNACHANonASCII, f)
		}
	}
	if len(e.Addenda) > 9999 {
		return fmt.Errorf("%w: %d", ErrNACHAAddenda, len(e.Addenda))
	}
	for _, a := range e.Addenda {
		if len(a) > 80 {
			return fmt.Errorf("%w: addenda line exceeds 80", ErrNACHAFieldTooLong)
		}
		if !nachaPrintable(a) {
			return fmt.Errorf("%w: addenda line", ErrNACHANonASCII)
		}
	}
	return nil
}

// NACHAFileConfig is the file header: who sends the file to whom.
// ImmediateDestination/Origin are 9-digit routing numbers (check digit
// validated); the header carries them as blank + the 9-digit routing
// (10 chars) per the NACHA layout.
type NACHAFileConfig struct {
	ImmediateDestination string
	ImmediateOrigin      string
	DestinationName      string
	OriginName           string
	// CreationTime defaults to now (UTC).
	CreationTime time.Time
	// FileIDModifier defaults to "A".
	FileIDModifier string
	ReferenceCode  string
}

func (c NACHAFileConfig) validate() (dest9, orig9 string, err error) {
	fail := func(format string, args ...any) (string, string, error) {
		return "", "", fmt.Errorf("%w: %s", ErrNACHAConfig, fmt.Sprintf(format, args...))
	}
	if _, err := NACHACheckDigit(c.ImmediateDestination); err != nil {
		return fail("immediate destination: %v", err)
	}
	if _, err := NACHACheckDigit(c.ImmediateOrigin); err != nil {
		return fail("immediate origin: %v", err)
	}
	if len(c.DestinationName) > 23 {
		return fail("destination name exceeds 23")
	}
	if len(c.OriginName) > 23 {
		return fail("origin name exceeds 23")
	}
	if !nachaPrintable(c.DestinationName) || !nachaPrintable(c.OriginName) {
		return fail("destination/origin name must be printable ASCII")
	}
	mod := c.FileIDModifier
	if mod == "" {
		mod = "A"
	}
	if len(mod) != 1 || !nachaPrintable(mod) {
		return fail("file ID modifier must be one printable ASCII character")
	}
	if len(c.ReferenceCode) > 8 {
		return fail("reference code exceeds 8")
	}
	if !nachaPrintable(c.ReferenceCode) {
		return fail("reference code must be printable ASCII")
	}
	return c.ImmediateDestination, c.ImmediateOrigin, nil
}

// NACHABatchConfig is the batch header: the originator and the SEC
// class of its entries.
type NACHABatchConfig struct {
	// ServiceClassCode: "200" mixed, "220" credits only, "225" debits
	// only.
	ServiceClassCode string
	CompanyName      string
	// CompanyDiscretionaryData is optional free text (≤20).
	CompanyDiscretionaryData string
	CompanyID                string
	// EntryClassCode: "CCD" or "PPD".
	EntryClassCode string
	// CompanyEntryDescription: e.g. "VENDOR PAY" (≤10).
	CompanyEntryDescription string
	// CompanyDescriptiveDate: optional (≤6), e.g. "101525".
	CompanyDescriptiveDate string
	// EffectiveEntryDate: the date the ODFI posts the entries; zero
	// means today (UTC).
	EffectiveEntryDate time.Time
	// OriginatingDFI: the ODFI's 8-digit transit number.
	OriginatingDFI string
	BatchNumber    int
}

func (c NACHABatchConfig) validate() error {
	if !nachaServiceClasses[c.ServiceClassCode] {
		return fmt.Errorf("%w: service class %q", ErrNACHAConfig, c.ServiceClassCode)
	}
	if !nachaSECCodes[c.EntryClassCode] {
		return fmt.Errorf("%w: SEC code %q", ErrNACHAConfig, c.EntryClassCode)
	}
	if !nachaAllDigits(c.OriginatingDFI, 8) {
		return fmt.Errorf("%w: originating DFI %q", ErrNACHAConfig, c.OriginatingDFI)
	}
	if c.BatchNumber < 1 || c.BatchNumber > 9999999 {
		return fmt.Errorf("%w: batch number %d", ErrNACHAConfig, c.BatchNumber)
	}
	for _, f := range []struct {
		name  string
		value string
		width int
	}{
		{"company name", c.CompanyName, 16},
		{"company discretionary data", c.CompanyDiscretionaryData, 20},
		{"company ID", c.CompanyID, 10},
		{"company entry description", c.CompanyEntryDescription, 10},
		{"company descriptive date", c.CompanyDescriptiveDate, 6},
	} {
		if len(f.value) > f.width {
			return fmt.Errorf("%w: %s exceeds %d", ErrNACHAConfig, f.name, f.width)
		}
		if !nachaPrintable(f.value) {
			return fmt.Errorf("%w: %s must be printable ASCII", ErrNACHAConfig, f.name)
		}
	}
	return nil
}

// NACHABatch is one batch: its header config plus entries.
type NACHABatch struct {
	Config  NACHABatchConfig
	Entries []NACHAEntry
}

// nachaEntryHash adds an entry's receiving DFI (first 8 of routing) to
// the running entry hash.
func nachaEntryHashAdd(sum int64, routing string) int64 {
	var v int64
	for i := 0; i < 8; i++ {
		v = v*10 + int64(routing[i]-'0')
	}
	return sum + v
}

// buildFileHeader renders record type "1".
func buildNACHAFileHeader(cfg NACHAFileConfig) (string, error) {
	dest9, orig9, err := cfg.validate()
	if err != nil {
		return "", err
	}
	created := cfg.CreationTime
	if created.IsZero() {
		created = time.Now().UTC()
	}
	mod := cfg.FileIDModifier
	if mod == "" {
		mod = "A"
	}
	var b strings.Builder
	b.WriteString("1")
	b.WriteString("01")
	b.WriteString(" " + dest9) // positions 4-13
	b.WriteString(" " + orig9) // positions 14-23
	b.WriteString(nachaYYMMDD(created))
	b.WriteString(fmt.Sprintf("%02d%02d", created.UTC().Hour(), created.UTC().Minute()))
	b.WriteString(mod)
	b.WriteString("094")
	b.WriteString("10")
	b.WriteString("1")
	destName, _ := nachaAlpha(cfg.DestinationName, 23)
	b.WriteString(destName)
	origName, _ := nachaAlpha(cfg.OriginName, 23)
	b.WriteString(origName)
	ref, _ := nachaAlpha(cfg.ReferenceCode, 8)
	b.WriteString(ref)
	return b.String(), nil
}

// buildBatchHeader renders record type "5".
func buildNACHABatchHeader(cfg NACHABatchConfig, effective time.Time) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("5")
	b.WriteString(cfg.ServiceClassCode)
	name, _ := nachaAlpha(cfg.CompanyName, 16)
	b.WriteString(name)
	disc, _ := nachaAlpha(cfg.CompanyDiscretionaryData, 20)
	b.WriteString(disc)
	companyID, _ := nachaAlpha(cfg.CompanyID, 10)
	b.WriteString(companyID)
	b.WriteString(cfg.EntryClassCode)
	desc, _ := nachaAlpha(cfg.CompanyEntryDescription, 10)
	b.WriteString(desc)
	date, _ := nachaAlpha(cfg.CompanyDescriptiveDate, 6)
	b.WriteString(date)
	b.WriteString(nachaYYMMDD(effective))
	b.WriteString("   ") // settlement date: inserted by the bank
	b.WriteString("1")   // originator status code
	b.WriteString(cfg.OriginatingDFI)
	batchNum, _ := nachaNumeric(int64(cfg.BatchNumber), 7)
	b.WriteString(batchNum)
	return b.String(), nil
}

// buildEntryDetail renders record type "6". trace is the full 15-digit
// trace number.
func buildNACHAEntryDetail(e NACHAEntry, trace string) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("6")
	b.WriteString(e.TransactionCode)
	b.WriteString(e.Routing[:8])
	b.WriteByte(nachaCheckDigitOf8(e.Routing[:8]))
	acct, _ := nachaAlpha(e.AccountNumber, 17)
	b.WriteString(acct)
	amt, _ := nachaAmount(e.AmountCents)
	b.WriteString(amt)
	iid, _ := nachaAlpha(e.IndividualID, 15)
	b.WriteString(iid)
	iname, _ := nachaAlpha(e.IndividualName, 22)
	b.WriteString(iname)
	disc, _ := nachaAlpha(e.Discretionary, 2)
	b.WriteString(disc)
	if len(e.Addenda) > 0 {
		b.WriteString("1")
	} else {
		b.WriteString("0")
	}
	b.WriteString(trace)
	return b.String(), nil
}

// buildAddenda renders one type "05" addenda record: the payment
// related info plus the addenda/entry detail sequence numbers.
func buildNACHAAddenda(info string, addendaSeq, entrySeq int) (string, error) {
	var b strings.Builder
	b.WriteString("7")
	b.WriteString("05")
	line, _ := nachaAlpha(info, 80)
	b.WriteString(line)
	aseq, _ := nachaNumeric(int64(addendaSeq), 4)
	b.WriteString(aseq)
	eseq, _ := nachaNumeric(int64(entrySeq), 7)
	b.WriteString(eseq)
	return b.String(), nil
}

// buildBatchControl renders record type "8".
func buildNACHABatchControl(cfg NACHABatchConfig, entryAddendaCount int, entryHash, debitTotal, creditTotal int64) (string, error) {
	var b strings.Builder
	b.WriteString("8")
	b.WriteString(cfg.ServiceClassCode)
	cnt, _ := nachaNumeric(int64(entryAddendaCount), 6)
	b.WriteString(cnt)
	hash, _ := nachaNumeric(entryHash%10000000000, 10)
	b.WriteString(hash)
	debit, _ := nachaNumeric(debitTotal, 12)
	b.WriteString(debit)
	credit, _ := nachaNumeric(creditTotal, 12)
	b.WriteString(credit)
	companyID, _ := nachaAlpha(cfg.CompanyID, 10)
	b.WriteString(companyID)
	b.WriteString(strings.Repeat(" ", 19)) // message authentication code
	b.WriteString(strings.Repeat(" ", 6))  // reserved
	b.WriteString(cfg.OriginatingDFI)
	batchNum, _ := nachaNumeric(int64(cfg.BatchNumber), 7)
	b.WriteString(batchNum)
	return b.String(), nil
}

// buildFileControl renders record type "9".
func buildNACHAFileControl(batchCount, blockCount, entryAddendaCount int, entryHash, debitTotal, creditTotal int64) (string, error) {
	var b strings.Builder
	b.WriteString("9")
	bc, _ := nachaNumeric(int64(batchCount), 6)
	b.WriteString(bc)
	blc, _ := nachaNumeric(int64(blockCount), 6)
	b.WriteString(blc)
	cnt, _ := nachaNumeric(int64(entryAddendaCount), 8)
	b.WriteString(cnt)
	hash, _ := nachaNumeric(entryHash%10000000000, 10)
	b.WriteString(hash)
	debit, _ := nachaNumeric(debitTotal, 12)
	b.WriteString(debit)
	credit, _ := nachaNumeric(creditTotal, 12)
	b.WriteString(credit)
	b.WriteString(strings.Repeat(" ", 39)) // reserved
	return b.String(), nil
}

// checkDirectionAllowed enforces the batch service class against the
// entry's direction: 220 accepts credits only, 225 debits only, 200
// both.
func checkDirectionAllowed(serviceClass string, dir NACHAEntryDirection) error {
	switch serviceClass {
	case "220":
		if dir != NACHAEntryCredit {
			return fmt.Errorf("%w: debit entry in credits-only batch (220)", ErrNACHAServiceClass)
		}
	case "225":
		if dir != NACHAEntryDebit {
			return fmt.Errorf("%w: credit entry in debits-only batch (225)", ErrNACHAServiceClass)
		}
	}
	return nil
}

// GenerateNACHAFile builds a complete NACHA file: file header, one
// batch block per batch (header / entries / addenda / control), file
// control, and "9" block padding. Lines are joined with "\n" (no
// trailing newline); every line is exactly 94 characters.
//
// Direction is validated twice: each entry's transaction code must be
// compatible with its batch's service class, and the batch control's
// debit/credit totals are split by that same direction — the totals
// always reconcile with the entry list.
func GenerateNACHAFile(cfg NACHAFileConfig, batches []NACHABatch) (string, error) {
	if len(batches) == 0 {
		return "", ErrNACHAEmpty
	}
	var lines []string
	header, err := buildNACHAFileHeader(cfg)
	if err != nil {
		return "", err
	}
	lines = append(lines, header)

	var (
		fileEntryAddenda int
		fileHash        int64
		fileDebit       int64
		fileCredit      int64
	)
	for _, batch := range batches {
		if len(batch.Entries) == 0 {
			return "", fmt.Errorf("%w: batch %d", ErrNACHAEmpty, batch.Config.BatchNumber)
		}
		if err := batch.Config.validate(); err != nil {
			return "", err
		}
		effective := batch.Config.EffectiveEntryDate
		if effective.IsZero() {
			effective = time.Now().UTC()
		}
		bh, err := buildNACHABatchHeader(batch.Config, effective)
		if err != nil {
			return "", err
		}
		lines = append(lines, bh)

		var (
			batchEntryAddenda int
			batchHash         int64
			batchDebit        int64
			batchCredit       int64
		)
		for i, e := range batch.Entries {
			if err := e.validate(); err != nil {
				return "", fmt.Errorf("batch %d entry %d: %w", batch.Config.BatchNumber, i+1, err)
			}
			dir, err := e.Direction()
			if err != nil {
				return "", fmt.Errorf("batch %d entry %d: %w", batch.Config.BatchNumber, i+1, err)
			}
			if err := checkDirectionAllowed(batch.Config.ServiceClassCode, dir); err != nil {
				return "", fmt.Errorf("batch %d entry %d: %w", batch.Config.BatchNumber, i+1, err)
			}
			trace := fmt.Sprintf("%s%07d", batch.Config.OriginatingDFI, i+1)
			detail, err := buildNACHAEntryDetail(e, trace)
			if err != nil {
				return "", err
			}
			lines = append(lines, detail)
			batchEntryAddenda++
			for j, a := range e.Addenda {
				rec, err := buildNACHAAddenda(a, j+1, i+1)
				if err != nil {
					return "", err
				}
				lines = append(lines, rec)
				batchEntryAddenda++
			}
			batchHash = nachaEntryHashAdd(batchHash, e.Routing)
			if dir == NACHAEntryDebit {
				batchDebit += e.AmountCents
			} else {
				batchCredit += e.AmountCents
			}
		}
		bc, err := buildNACHABatchControl(batch.Config, batchEntryAddenda, batchHash, batchDebit, batchCredit)
		if err != nil {
			return "", err
		}
		lines = append(lines, bc)

		fileEntryAddenda += batchEntryAddenda
		fileHash += batchHash
		fileDebit += batchDebit
		fileCredit += batchCredit
	}

	// Block padding: the file must hold a multiple of 10 records,
	// counting the file control record itself.
	withControl := len(lines) + 1
	pad := (nachaBlockLen - withControl%nachaBlockLen) % nachaBlockLen
	blockCount := (withControl + pad) / nachaBlockLen
	fc, err := buildNACHAFileControl(len(batches), blockCount, fileEntryAddenda, fileHash, fileDebit, fileCredit)
	if err != nil {
		return "", err
	}
	lines = append(lines, fc)
	for i := 0; i < pad; i++ {
		lines = append(lines, strings.Repeat("9", nachaRecordLen))
	}
	for _, l := range lines {
		if len(l) != nachaRecordLen {
			return "", fmt.Errorf("%w: record of length %d", ErrNACHAConfig, len(l))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// NACHAReceiver names the receiving account for a payout conversion.
type NACHAReceiver struct {
	// Routing is the receiver's 9-digit routing number.
	Routing string
	// AccountNumber is the receiver's account number (≤17 chars).
	AccountNumber string
	// IndividualName is the receiver's name (≤22 chars, ASCII).
	IndividualName string
	// Savings selects transaction code 32 over 22 (checking).
	Savings bool
}

// PayoutToNACHAEntry converts a payout instruction into a NACHA entry
// detail. A payout is money leaving the ledger, so the ACH direction is
// always a credit to the receiver: 22 (checking) or 32 (savings). The
// payout ID becomes the individual identification number (≤15 chars).
func PayoutToNACHAEntry(p PayoutInstruction, r NACHAReceiver) (NACHAEntry, error) {
	if p.AmountCents <= 0 {
		return NACHAEntry{}, fmt.Errorf("%w: payout %q amount %d", ErrNACHAAmount, p.ID, p.AmountCents)
	}
	code := "22"
	if r.Savings {
		code = "32"
	}
	e := NACHAEntry{
		TransactionCode: code,
		Routing:         r.Routing,
		AccountNumber:   r.AccountNumber,
		AmountCents:     p.AmountCents,
		IndividualID:    p.ID,
		IndividualName:  r.IndividualName,
	}
	if err := e.validate(); err != nil {
		return NACHAEntry{}, fmt.Errorf("payout %q: %w", p.ID, err)
	}
	return e, nil
}
