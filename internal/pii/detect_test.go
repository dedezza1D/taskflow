package pii

import (
	"strings"
	"testing"
)

func categories(fs []Finding) map[string]int { return Counts(fs) }

func TestLuhn(t *testing.T) {
	if !luhnOK("4111111111111111") {
		t.Fatal("valid Visa test number should pass Luhn")
	}
	if luhnOK("4111111111111112") {
		t.Fatal("invalid checksum should fail Luhn")
	}
}

func TestIBANMod97(t *testing.T) {
	if !ibanOK("DE89370400440532013000") {
		t.Fatal("valid DE IBAN should pass mod-97")
	}
	if !ibanOK("DE89 3704 0044 0532 0130 00") {
		t.Fatal("spaced valid DE IBAN should pass mod-97")
	}
	if ibanOK("DE89370400440532013001") {
		t.Fatal("corrupted IBAN should fail mod-97")
	}
}

func TestSteuerID(t *testing.T) {
	if !steuerIDOK("86095742719") {
		t.Fatal("known-valid test Steuer-ID should pass ISO 7064 MOD 11,10")
	}
	if steuerIDOK("86095742718") {
		t.Fatal("wrong check digit should fail")
	}
	if steuerIDOK("06095742719") {
		t.Fatal("leading zero is not a valid Steuer-ID")
	}
}

func TestCPF(t *testing.T) {
	if !cpfOK("111.444.777-35") {
		t.Fatal("known-valid CPF should pass mod-11 check digits")
	}
	if !cpfOK("11144477735") {
		t.Fatal("bare valid CPF should pass")
	}
	if cpfOK("111.444.777-36") {
		t.Fatal("wrong check digit should fail")
	}
	if cpfOK("111.111.111-11") {
		t.Fatal("all-same-digit CPF satisfies the arithmetic but is invalid")
	}
}

func TestCNPJ(t *testing.T) {
	if !cnpjOK("11.222.333/0001-81") {
		t.Fatal("known-valid CNPJ should pass mod-11 check digits")
	}
	if !cnpjOK("11222333000181") {
		t.Fatal("bare valid CNPJ should pass")
	}
	if cnpjOK("11.222.333/0001-82") {
		t.Fatal("wrong check digit should fail")
	}
	if cnpjOK("11111111111111") {
		t.Fatal("all-same-digit CNPJ must be rejected")
	}
}

func TestDetectBrazilianIDs(t *testing.T) {
	text := "cliente CPF 111.444.777-35 ou 11144477735\nempresa CNPJ 11.222.333/0001-81 (11222333000181)\n"
	got := categories(Detect(text))
	if got[CategoryCPF] != 2 {
		t.Fatalf("expected 2 cpf_br findings (formatted + bare), got %v", got)
	}
	if got[CategoryCNPJ] != 2 {
		t.Fatalf("expected 2 cnpj_br findings (formatted + bare), got %v", got)
	}
	// The bare forms must not double-report as phone/card/tax_id_de.
	if got[CategoryPhone] != 0 || got[CategoryCreditCard] != 0 || got[CategoryTaxIDDE] != 0 {
		t.Fatalf("brazilian IDs double-reported: %v", got)
	}
}

func TestRedactBrazilianIDs(t *testing.T) {
	out := Redact("failed for CPF 111.444.777-35 and CNPJ 11.222.333/0001-81")
	for _, leaked := range []string{"111.444.777-35", "11.222.333/0001-81"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("redacted output still contains %q: %s", leaked, out)
		}
	}
	for _, marker := range []string{"[REDACTED:cpf_br]", "[REDACTED:cnpj_br]"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("expected marker %s in output: %s", marker, out)
		}
	}
}

func TestDetectFindsValidatedCategories(t *testing.T) {
	text := "line one\n" +
		"contact: john.doe@example.com or 0341 2233445\n" +
		"card: 4111 1111 1111 1111\n" +
		"iban: DE89 3704 0044 0532 0130 00\n" +
		"tax: 86095742719\n"

	fs := Detect(text)
	got := categories(fs)

	for _, want := range []string{CategoryEmail, CategoryPhone, CategoryCreditCard, CategoryIBAN, CategoryTaxIDDE} {
		if got[want] == 0 {
			t.Fatalf("expected at least one %s finding, got %v", want, got)
		}
	}

	// Location sanity: the email lives on line 2.
	for _, f := range fs {
		if f.Category == CategoryEmail && f.Line != 2 {
			t.Fatalf("email should be on line 2, got line %d", f.Line)
		}
		if f.Length <= 0 || f.End <= f.Start {
			t.Fatalf("finding has degenerate location: %+v", f)
		}
	}
}

func TestDetectRejectsInvalidChecksums(t *testing.T) {
	text := "card: 4111 1111 1111 1112\niban: DE00 0000 0000 0000 0000 00\n"
	got := categories(Detect(text))
	if got[CategoryCreditCard] != 0 {
		t.Fatalf("Luhn-invalid number must not be reported as credit_card: %v", got)
	}
	if got[CategoryIBAN] != 0 {
		t.Fatalf("mod-97-invalid IBAN must not be reported: %v", got)
	}
}

func TestIBANNotDoubleReportedAsCardOrPhone(t *testing.T) {
	got := categories(Detect("DE89370400440532013000"))
	if got[CategoryIBAN] != 1 {
		t.Fatalf("expected exactly one iban finding, got %v", got)
	}
	if got[CategoryCreditCard] != 0 || got[CategoryPhone] != 0 {
		t.Fatalf("iban digits must not also report card/phone: %v", got)
	}
}

func TestFindingsCarryNoValues(t *testing.T) {
	// Structural guarantee: Finding has no value field, so any finding is just
	// category+location. This test guards the contract against regressions.
	fs := Detect("mail me at secret.person@example.com")
	if len(fs) == 0 {
		t.Fatal("expected a finding")
	}
	f := fs[0]
	_ = f.Category
	_ = f.Line
	_ = f.Start
	_ = f.End
	_ = f.Length
}

func TestRedact(t *testing.T) {
	in := "handler failed for john.doe@example.com with card 4111111111111111 (iban DE89370400440532013000)"
	out := Redact(in)

	for _, leaked := range []string{"john.doe@example.com", "4111111111111111", "DE89370400440532013000"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("redacted output still contains %q: %s", leaked, out)
		}
	}
	for _, marker := range []string{"[REDACTED:email]", "[REDACTED:credit_card]", "[REDACTED:iban]"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("expected marker %s in output: %s", marker, out)
		}
	}
	if Redact("no pii here") != "no pii here" {
		t.Fatal("clean strings must pass through unchanged")
	}
}

// OCR inserts stray spaces into long digit runs — the Portuguese tesseract model
// reliably turns "529.982.247-25" into "529.982 .247-25". These detectors run
// over OCR output, so a pattern that insists on exact punctuation silently
// misses real identifiers. The check digit, not the spacing, is what decides.
func TestDetectToleratesOCRSpacingNoise(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"cpf with a space before the dot group", "CPF: 529.982 .247-25", CategoryCPF},
		{"cpf with a space after a dot", "CPF: 529. 982.247-25", CategoryCPF},
		{"cpf with a space around the dash", "CPF: 529.982.247 - 25", CategoryCPF},
		{"cnpj with spacing noise", "CNPJ: 11.222 .333/0001-81", CategoryCNPJ},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for _, f := range Detect(tc.text) {
				if f.Category == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("%q: no %s finding — OCR spacing made it invisible", tc.text, tc.want)
			}
		})
	}
}

// The loosened shape must not become a licence to match nonsense: the mod-11
// check digit is still the gate.
func TestDetectStillRejectsBadCheckDigits(t *testing.T) {
	for _, text := range []string{
		"CPF: 529.982 .247-26", // last digit wrong
		"CPF: 111.111 .111-11", // all-same-digit run
	} {
		for _, f := range Detect(text) {
			if f.Category == CategoryCPF {
				t.Errorf("%q matched as a CPF despite an invalid check digit", text)
			}
		}
	}
}

// Brazilian phone numbers were invisible to the detector: both existing
// patterns are European-shaped, and the local one requires a leading zero that
// a Brazilian number never has. A tool that leads with CPF and the LGPD cannot
// miss the second most common identifier in the same document.
func TestDetectBrazilianPhones(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"mobile with parenthesised area code", "Telefone: (11) 98765-4321"},
		{"mobile without parentheses", "Contato 11 98765-4321"},
		{"mobile with only a space after the area code", "Contato 11 987654321"},
		{"landline", "Recepcao: (21) 3456-7890"},
		{"dotted separator", "Tel 11.98765.4321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var found bool
			for _, f := range Detect(tc.text) {
				if f.Category == CategoryPhone {
					found = true
				}
			}
			if !found {
				t.Fatalf("no phone detected in %q", tc.text)
			}
		})
	}
}

// The permissive shape is only safe while the check-digit categories keep
// their claim on the text. These are the strings most likely to be misread as
// a phone number, and each must come back as itself or as nothing at all.
func TestBrazilianPhoneDoesNotStealOtherCategories(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want string // the category that must win, "" when nothing should match
	}{
		{"valid CPF stays a CPF", "CPF 529.982.247-25", CategoryCPF},
		{"valid card stays a card", "Cartao 4111 1111 1111 1111", CategoryCreditCard},
		{"CEP is not a phone", "CEP 13100-000", ""},
		{"invalid CPF is not reported as a phone", "Numero 123.456.789-00", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cats := map[string]bool{}
			for _, f := range Detect(tc.text) {
				cats[f.Category] = true
			}
			if tc.want == "" {
				if cats[CategoryPhone] {
					t.Fatalf("%q was reported as a phone; got %v", tc.text, cats)
				}
				return
			}
			if !cats[tc.want] {
				t.Fatalf("%q did not yield %s; got %v", tc.text, tc.want, cats)
			}
			if cats[CategoryPhone] {
				t.Fatalf("%q was also reported as a phone; got %v", tc.text, cats)
			}
		})
	}
}

// A Brazilian mobile written with no separators at all is eleven digits, which
// is exactly the shape of a German Steuer-ID -- and roughly one such number in
// eleven satisfies its ISO 7064 check digit. When that happens the check digit
// wins and the number is filed under tax_id_de.
//
// This is the documented precedence working as designed rather than a defect:
// a validated identifier outranks a best-effort pattern, and the compliance
// verdict is the same either way, because both are personal data. It is
// recorded here so the next person to read a "Steuer-ID" in a Brazilian
// document knows what they are looking at.
func TestUnseparatedMobileCanCollideWithSteuerID(t *testing.T) {
	got := Detect("fone 11987654321")
	if len(got) != 1 || got[0].Category != CategoryTaxIDDE {
		t.Fatalf("expected the check-digit category to win, got %v", got)
	}

	// One digit later the check digit fails, and the phone pattern picks it up.
	got = Detect("fone 11987654322")
	if len(got) != 1 || got[0].Category != CategoryPhone {
		t.Fatalf("expected a phone, got %v", got)
	}
}
