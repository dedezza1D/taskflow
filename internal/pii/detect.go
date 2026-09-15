// Package pii holds the shared detector set — the low-false-positive core
// (Luhn-validated cards, mod-97-validated IBANs, check-digit-validated German
// Steuer-IDs, Brazilian CPF/CNPJ) plus best-effort email/phone.
//
// It is deliberately dependency-free and imported from BOTH sides of the
// compliance boundary:
//
//   - the pipeline's PII stage (internal/pipeline) uses Detect to produce
//     findings — category and location, NEVER the raw value; and
//   - the audit chokepoint (internal/worker.ScrubError) uses Redact so the same
//     patterns keep values out of task_executions.error, the DLQ, logs and spans.
//
// One pattern set, two enforcement points, zero drift between them.
//
// The upgrade path is a Presidio NER sidecar layered on top of (not replacing)
// this set; DetectorVersion is recorded in artifacts so reports state which
// detector produced them.
package pii

import (
	"regexp"
	"sort"
	"strings"
)

// DetectorVersion is stamped into findings and report artifacts.
const DetectorVersion = "regex-v2"

// Finding locates a detection WITHOUT carrying the matched value — findings and
// reports must not become a secondary PII store. Line is 1-based; Start/End are
// byte offsets within that line.
type Finding struct {
	Category string `json:"category"`
	Line     int    `json:"line"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Length   int    `json:"length"`
}

// Categories produced by this detector set.
const (
	CategoryEmail      = "email"
	CategoryPhone      = "phone"
	CategoryCreditCard = "credit_card"
	CategoryIBAN       = "iban"
	CategoryTaxIDDE    = "tax_id_de"
	CategoryCPF        = "cpf_br"
	CategoryCNPJ       = "cnpj_br"
)

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	// 13–19 digits allowing single space/dash separators; Luhn-validated below.
	cardRe = regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)
	// Country code + 2 check digits + BBAN, optionally grouped by spaces;
	// normalized and mod-97-validated below.
	ibanRe = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,32}\b`)
	// Best-effort phone: international (+.. / 00..) or local leading-0 forms.
	phoneIntlRe  = regexp.MustCompile(`(?:\+|\b00)[1-9]\d(?:[ \-/]?\d){6,13}`)
	phoneLocalRe = regexp.MustCompile(`\b0\d{2,4}[ \-/]?\d{4,10}\b`)
	// Brazilian: (11) 98765-4321, 11 98765-4321, 11 3456-7890. Neither pattern
	// above can match one -- the local form insists on a leading zero, which is
	// a German trunk prefix and is never written in a Brazilian number. That gap
	// meant the most common personal identifier in a Brazilian document, after
	// the CPF, went unreported by a tool whose whole subject is the LGPD.
	//
	// Permissive on purpose, and safe to be: phone is matched last, so anything
	// a check digit already claimed as a card, CPF, CNPJ or IBAN keeps its
	// category. The area code excludes a leading 0, so a CEP cannot match.
	phoneBRRe = regexp.MustCompile(`(?:\([1-9]\d\)|\b[1-9]\d)[ .\-]?9?\d{4}[ .\-]?\d{4}\b`)
	// 11 digits; validated with the official ISO 7064 MOD 11,10 check digit.
	taxIDRe = regexp.MustCompile(`\b\d{11}\b`)
	// Brazilian CPF, formatted (123.456.789-09); mod-11 double check digit. The
	// bare 11-digit form shares taxIDRe's shape and is validated separately.
	//
	// Whitespace is tolerated around the separators because these patterns run
	// over OCR output, and OCR inserts stray spaces into long digit runs —
	// observed with the Portuguese model turning "529.982.247-25" into
	// "529.982 .247-25". Being strict here costs a MISSED CPF, which in a tool
	// built to find personal data is the expensive direction to fail. Loosening
	// the shape is safe because the mod-11 check digit, not the punctuation, is
	// what actually decides: that is the same two-stage arrangement the card and
	// IBAN patterns already use.
	cpfRe = regexp.MustCompile(`\b\d{3}\s?\.\s?\d{3}\s?\.\s?\d{3}\s?-\s?\d{2}\b`)
	// Brazilian CNPJ, formatted (12.345.678/0001-95) and bare 14-digit forms;
	// mod-11 double check digit.
	cnpjRe     = regexp.MustCompile(`\b\d{2}\s?\.\s?\d{3}\s?\.\s?\d{3}\s?/\s?\d{4}\s?-\s?\d{2}\b`)
	cnpjBareRe = regexp.MustCompile(`\b\d{14}\b`)

	digitsOnlyRe = regexp.MustCompile(`\D`)
	spaceRe      = regexp.MustCompile(` `)
)

type span struct {
	start, end int
	category   string
}

// Detect returns findings (category + location only) for text.
//
// Precedence when candidates overlap — IBAN > formatted CNPJ > credit card >
// bare CNPJ > formatted CPF > German tax ID > bare CPF > phone — so a
// validated IBAN is never double-reported as the card/phone number its digit
// run also resembles. Punctuated Brazilian forms rank above the ambiguous
// bare-digit runs they share with cards (14 digits) and Steuer-IDs (11
// digits); for a bare 11-digit run valid under both checksums, tax_id_de
// wins. Email is textually disjoint from the numeric set.
func Detect(text string) []Finding {
	spans := detectSpans(text)
	lineStarts := computeLineStarts(text)

	out := make([]Finding, 0, len(spans))
	for _, sp := range spans {
		line, lineStart := lineFor(lineStarts, sp.start)
		out = append(out, Finding{
			Category: sp.category,
			Line:     line,
			Start:    sp.start - lineStart,
			End:      sp.end - lineStart,
			Length:   sp.end - sp.start,
		})
	}
	return out
}

// Counts aggregates findings by category.
func Counts(findings []Finding) map[string]int {
	c := map[string]int{}
	for _, f := range findings {
		c[f.Category]++
	}
	return c
}

// Redact replaces every detected value in s with [REDACTED:<category>]. This is
// the primitive behind worker.ScrubError (C2/C3): the same validated patterns
// that produce findings also keep values out of everything durable.
func Redact(s string) string {
	spans := detectSpans(s)
	if len(spans) == 0 {
		return s
	}
	// Replace back-to-front so earlier offsets stay valid.
	b := []byte(s)
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		repl := []byte("[REDACTED:" + sp.category + "]")
		b = append(b[:sp.start], append(repl, b[sp.end:]...)...)
	}
	return string(b)
}

func detectSpans(text string) []span {
	var spans []span

	// IBAN first (highest precedence).
	for _, loc := range ibanRe.FindAllStringIndex(text, -1) {
		cand := text[loc[0]:loc[1]]
		if ibanOK(cand) {
			spans = append(spans, span{loc[0], loc[1], CategoryIBAN})
		}
	}

	spans = addNonOverlapping(spans, findValidated(text, cnpjRe, CategoryCNPJ, cnpjOK))

	spans = addNonOverlapping(spans, findValidated(text, cardRe, CategoryCreditCard, func(m string) bool {
		d := digitsOnlyRe.ReplaceAllString(m, "")
		return len(d) >= 13 && len(d) <= 19 && !allSameDigit(d) && luhnOK(d)
	}))

	spans = addNonOverlapping(spans, findValidated(text, cnpjBareRe, CategoryCNPJ, cnpjOK))

	spans = addNonOverlapping(spans, findValidated(text, cpfRe, CategoryCPF, cpfOK))

	spans = addNonOverlapping(spans, findValidated(text, taxIDRe, CategoryTaxIDDE, steuerIDOK))

	spans = addNonOverlapping(spans, findValidated(text, taxIDRe, CategoryCPF, cpfOK))

	spans = addNonOverlapping(spans, findValidated(text, phoneIntlRe, CategoryPhone, nil))
	spans = addNonOverlapping(spans, findValidated(text, phoneLocalRe, CategoryPhone, nil))
	spans = addNonOverlapping(spans, findValidated(text, phoneBRRe, CategoryPhone, nil))

	spans = addNonOverlapping(spans, findValidated(text, emailRe, CategoryEmail, nil))

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans
}

func findValidated(text string, re *regexp.Regexp, category string, valid func(string) bool) []span {
	var out []span
	for _, loc := range re.FindAllStringIndex(text, -1) {
		m := text[loc[0]:loc[1]]
		if valid == nil || valid(m) {
			out = append(out, span{loc[0], loc[1], category})
		}
	}
	return out
}

func addNonOverlapping(existing, candidates []span) []span {
	for _, c := range candidates {
		overlaps := false
		for _, e := range existing {
			if c.start < e.end && e.start < c.end {
				overlaps = true
				break
			}
		}
		if !overlaps {
			existing = append(existing, c)
		}
	}
	return existing
}

// allSameDigit reports whether every character in a digit string is identical
// (e.g. "0000..."). Such runs are a Luhn blind spot — all-zeros sums to 0 and
// passes — but are never real card numbers, and they turn up as the digit body
// of masked/invalid IBANs. Rejecting them kills that false positive.
func allSameDigit(digits string) bool {
	for i := 1; i < len(digits); i++ {
		if digits[i] != digits[0] {
			return false
		}
	}
	return len(digits) > 0
}

// luhnOK validates a digit string with the Luhn checksum (payment cards).
func luhnOK(digits string) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// ibanOK validates an IBAN candidate with the ISO 13616 mod-97 check: strip
// spaces, move the first four characters to the end, expand letters to 10..35,
// and the resulting number mod 97 must equal 1.
func ibanOK(candidate string) bool {
	s := spaceRe.ReplaceAllString(candidate, "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			v := int(c-'A') + 10
			rem = (rem*100 + v) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// cpfOK validates a Brazilian CPF candidate (formatted or bare) with its two
// official mod-11 check digits. All-same-digit CPFs (111.111.111-11 etc.)
// satisfy the arithmetic but are explicitly invalid, hence the allSameDigit
// rejection.
func cpfOK(candidate string) bool {
	d := digitsOnlyRe.ReplaceAllString(candidate, "")
	if len(d) != 11 || allSameDigit(d) {
		return false
	}
	return cpfCheckDigit(d, 9) == int(d[9]-'0') && cpfCheckDigit(d, 10) == int(d[10]-'0')
}

// cpfCheckDigit computes the CPF mod-11 check digit over d[:n] with weights
// n+1 down to 2.
func cpfCheckDigit(d string, n int) int {
	sum := 0
	for i := 0; i < n; i++ {
		sum += int(d[i]-'0') * (n + 1 - i)
	}
	r := sum % 11
	if r < 2 {
		return 0
	}
	return 11 - r
}

var (
	cnpjWeights1 = []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
	cnpjWeights2 = []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2}
)

// cnpjOK validates a Brazilian CNPJ candidate (formatted or bare) with its two
// official mod-11 check digits.
func cnpjOK(candidate string) bool {
	d := digitsOnlyRe.ReplaceAllString(candidate, "")
	if len(d) != 14 || allSameDigit(d) {
		return false
	}
	return cnpjCheckDigit(d, cnpjWeights1) == int(d[12]-'0') &&
		cnpjCheckDigit(d, cnpjWeights2) == int(d[13]-'0')
}

func cnpjCheckDigit(d string, weights []int) int {
	sum := 0
	for i, w := range weights {
		sum += int(d[i]-'0') * w
	}
	r := sum % 11
	if r < 2 {
		return 0
	}
	return 11 - r
}

// steuerIDOK validates a German Steuer-ID candidate (11 digits) with its
// official ISO 7064 MOD 11,10 check digit.
func steuerIDOK(candidate string) bool {
	if len(candidate) != 11 || strings.HasPrefix(candidate, "0") {
		return false
	}
	product := 10
	for i := 0; i < 10; i++ {
		d := int(candidate[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		sum := (d + product) % 10
		if sum == 0 {
			sum = 10
		}
		product = (sum * 2) % 11
	}
	check := 11 - product
	if check == 10 {
		check = 0
	}
	return int(candidate[10]-'0') == check
}

func computeLineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineFor returns the 1-based line number and the byte offset of that line's
// start for a given absolute offset.
func lineFor(lineStarts []int, offset int) (int, int) {
	idx := sort.Search(len(lineStarts), func(i int) bool { return lineStarts[i] > offset }) - 1
	if idx < 0 {
		idx = 0
	}
	return idx + 1, lineStarts[idx]
}
