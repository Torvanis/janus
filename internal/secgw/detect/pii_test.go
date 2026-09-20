package detect

import (
	"strings"
	"testing"
)

func classesOf(ms []Match) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.RuleID)
	}
	return out
}

func onlyClass(t *testing.T, ms []Match, class PIIClass) []Match {
	t.Helper()
	var out []Match
	for _, m := range ms {
		if m.RuleID == string(class) {
			out = append(out, m)
		}
	}
	return out
}

func assertClassText(t *testing.T, text string, class PIIClass, want string, sev Severity) {
	t.Helper()
	ms := NewPIIDetector(PIIOptions{}).Detect(text)
	got := onlyClass(t, ms, class)
	if len(got) != 1 {
		t.Fatalf("%q: want exactly one %s match, got %v", text, class, classesOf(ms))
	}
	m := got[0]
	if m.Text != want {
		t.Errorf("%q: Text = %q, want %q", text, m.Text, want)
	}
	if text[m.Offset:m.Offset+m.Length] != want {
		t.Errorf("%q: Offset/Length span = %q, want %q", text, text[m.Offset:m.Offset+m.Length], want)
	}
	if m.Kind != KindPII {
		t.Errorf("Kind = %q, want %q", m.Kind, KindPII)
	}
	if m.Severity != sev {
		t.Errorf("Severity = %q, want %q", m.Severity, sev)
	}
	if m.Replacement != "[REDACTED:"+string(class)+"]" {
		t.Errorf("Replacement = %q", m.Replacement)
	}
}

func assertNoClass(t *testing.T, text string, class PIIClass) {
	t.Helper()
	ms := NewPIIDetector(PIIOptions{}).Detect(text)
	if got := onlyClass(t, ms, class); len(got) != 0 {
		t.Errorf("%q: unexpected %s match %+v", text, class, got)
	}
}

func assertNothing(t *testing.T, text string) {
	t.Helper()
	if ms := NewPIIDetector(PIIOptions{}).Detect(text); len(ms) != 0 {
		t.Errorf("%q: expected no matches, got %v", text, classesOf(ms))
	}
}

// ---- Luhn ------------------------------------------------------------------

func TestLuhn(t *testing.T) {
	for _, ok := range []string{"4111111111111111", "5500000000000004", "340000000000009", "6011000000000004", "79927398713"} {
		if !Luhn(ok) {
			t.Errorf("Luhn(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"4111111111111112", "79927398710", "1234567890123456", "", "1", "4111-1111"} {
		if Luhn(bad) {
			t.Errorf("Luhn(%q) = true, want false", bad)
		}
	}
}

// ---- PAN -------------------------------------------------------------------

func TestPII_PAN_Positives(t *testing.T) {
	cases := []struct{ text, want string }{
		{"visa 4111 1111 1111 1111 end", "4111 1111 1111 1111"},
		{"visa 4111111111111111 end", "4111111111111111"},
		{"visa 4111-1111-1111-1111 end", "4111-1111-1111-1111"},
		{"mc 5500 0000 0000 0004 end", "5500 0000 0000 0004"},
		{"mc 5500000000000004", "5500000000000004"},
		{"amex 3400 0000 0000 009 end", "3400 0000 0000 009"},
		{"amex 340000000000009", "340000000000009"},
		{"disc 6011 0000 0000 0004", "6011 0000 0000 0004"},
		{"disc 6011000000000004.", "6011000000000004"},
	}
	for _, c := range cases {
		assertClassText(t, c.text, ClassPAN, c.want, SeverityHigh)
	}
}

func TestPII_PAN_Negatives(t *testing.T) {
	for _, text := range []string{
		"4111 1111 1111 1112",  // Luhn failure
		"1234 5678 9012 3452",  // Luhn ok but no plausible IIN
		"411111111111",         // too short (12 digits)
		"41111111111111111111", // too long (20 digits)
		"4111  1111 1111 1111", // double space is not a separator
		"order 1234567890",
	} {
		assertNoClass(t, text, ClassPAN)
	}
}

// ---- IBAN ------------------------------------------------------------------

func TestIBANValid(t *testing.T) {
	for _, ok := range []string{
		"DE89 3704 0044 0532 0130 00",
		"DE89370400440532013000",
		"GB82 WEST 1234 5698 7654 32",
		"FR14 2004 1010 0505 0001 3M02 606",
		"NL91 ABNA 0417 1643 00",
		"BE68 5390 0754 7034",
	} {
		if !IBANValid(ok) {
			t.Errorf("IBANValid(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"DE89 3704 0044 0532 0130 01", // checksum
		"DE89 3704 0044 0532 0130",    // wrong length for DE
		"ZZ89 3704 0044 0532 0130 00", // unknown country
		"GB82 west 1234 5698 7654 32", // lower case
		"",
		"DE",
	} {
		if IBANValid(bad) {
			t.Errorf("IBANValid(%q) = true, want false", bad)
		}
	}
}

func TestPII_IBAN_Positives(t *testing.T) {
	cases := []struct{ text, want string }{
		{"send to DE89 3704 0044 0532 0130 00 today", "DE89 3704 0044 0532 0130 00"},
		{"send to DE89370400440532013000 today", "DE89370400440532013000"},
		{"GB82 WEST 1234 5698 7654 32", "GB82 WEST 1234 5698 7654 32"},
		{"FR14 2004 1010 0505 0001 3M02 606.", "FR14 2004 1010 0505 0001 3M02 606"},
	}
	for _, c := range cases {
		assertClassText(t, c.text, ClassIBAN, c.want, SeverityHigh)
	}
	// An IBAN's digit groups must not also be reported as a PAN or phone.
	ms := NewPIIDetector(PIIOptions{}).Detect("DE89 3704 0044 0532 0130 00")
	if len(ms) != 1 {
		t.Errorf("IBAN produced overlapping matches: %v", classesOf(ms))
	}
}

func TestPII_IBAN_Negatives(t *testing.T) {
	for _, text := range []string{
		"DE89 3704 0044 0532 0130 01", // bad checksum
		"ZZ89 3704 0044 0532 0130 00", // unknown country
		"DE89 3704 0044 0532 0130",    // too short
		"DE89370400440532013000123",   // longer alphanumeric run
		"GB82 WEST 1234 5698 7654 3X", // checksum breaks
		"ABCD1234 is a product code",  // letters but not IBAN
	} {
		assertNoClass(t, text, ClassIBAN)
	}
}

// ---- SSN -------------------------------------------------------------------

func TestPII_SSN_Positives(t *testing.T) {
	cases := []struct{ text, want string }{
		{"ssn 123-45-6789 on file", "123-45-6789"},
		{"SSN: 001-01-0001", "001-01-0001"},
		{"(899-99-9999)", "899-99-9999"},
	}
	for _, c := range cases {
		assertClassText(t, c.text, ClassSSNUS, c.want, SeverityHigh)
	}
}

func TestPII_SSN_Negatives(t *testing.T) {
	for _, text := range []string{
		"000-12-3456",
		"666-12-3456",
		"123-00-4567",
		"123-45-0000",
		"987-65-4320", // area 900-999
		"2026-09-07",  // timestamp
		"123456789",   // bare, disallowed by default
		"1123-45-6789",
	} {
		assertNoClass(t, text, ClassSSNUS)
	}
}

func TestPII_SSN_AllowBare(t *testing.T) {
	d := NewPIIDetector(PIIOptions{AllowBareSSN: true})
	ms := onlyClass(t, d.Detect("bare 123456789 here"), ClassSSNUS)
	if len(ms) != 1 || ms[0].Text != "123456789" {
		t.Fatalf("AllowBareSSN: got %+v", ms)
	}
	for _, bad := range []string{"000123456", "666123456", "912345678", "123004567", "123450000", "1234567890"} {
		if got := onlyClass(t, d.Detect(bad), ClassSSNUS); len(got) != 0 {
			t.Errorf("AllowBareSSN %q: unexpected %+v", bad, got)
		}
	}
}

// ---- NPI -------------------------------------------------------------------

func TestPII_NPI_Positives(t *testing.T) {
	// Well-known valid NPIs: 1234567893 (CMS example), 1245319599, 1679576722.
	for _, n := range []string{"1234567893", "1245319599", "1679576722"} {
		if !Luhn("80840" + n) {
			t.Fatalf("test fixture %s is not a valid NPI", n)
		}
		assertClassText(t, "provider npi "+n+" billed", ClassNPI, n, SeverityMedium)
	}
}

func TestPII_NPI_Negatives(t *testing.T) {
	for _, text := range []string{
		"1234567890",      // fails NPI Luhn (order number)
		"1234567894",      // off by one
		"123456789",       // 9 digits
		"12345678930",     // 11 digits
		"+1234567893",     // phone context: leading +
		"1234567893-1234", // phone-ish context: trailing hyphen
	} {
		assertNoClass(t, text, ClassNPI)
	}
}

// ---- Email -----------------------------------------------------------------

func TestPII_Email_Positives(t *testing.T) {
	cases := []struct{ text, want string }{
		{"mail alice@example.com now", "alice@example.com"},
		{"first.last+tag@sub.example.co.uk,", "first.last+tag@sub.example.co.uk"},
		{"https://bob@example.org/path?x=1", "bob@example.org"}, // inside a URL
		{"<carol_1@mail-server.io>", "carol_1@mail-server.io"},
	}
	for _, c := range cases {
		assertClassText(t, c.text, ClassEmail, c.want, SeverityLow)
	}
}

func TestPII_Email_Negatives(t *testing.T) {
	for _, text := range []string{
		"alice@localhost",   // no TLD
		"alice@example.c",   // 1-letter TLD
		"alice@example.123", // numeric TLD
		"@example.com",
		"alice at example dot com",
		"alice@-bad.com",
	} {
		assertNoClass(t, text, ClassEmail)
	}
}

// ---- Phone -----------------------------------------------------------------

func TestPII_Phone_Positives(t *testing.T) {
	cases := []struct{ text, want string }{
		{"call +14155552671 now", "+14155552671"},
		{"call +442071838750.", "+442071838750"},
		{"call (415) 555-2671 now", "(415) 555-2671"},
		{"call 415-555-2671 now", "415-555-2671"},
		{"call 415.555.2671 now", "415.555.2671"},
		{"call 415 555 2671 now", "415 555 2671"},
	}
	for _, c := range cases {
		assertClassText(t, c.text, ClassPhone, c.want, SeverityLow)
	}
}

func TestPII_Phone_Negatives(t *testing.T) {
	for _, text := range []string{
		"order 1234567890",  // bare 10 digits: not phone
		"order 4155552671",  // bare 10 digits: not phone
		"+0123456789",       // E.164 cannot start with 0
		"+1234567",          // too short
		"415-555-267",       // short line number
		"id 12345-678-9012", // not NANP shape
	} {
		assertNoClass(t, text, ClassPhone)
	}
}

// ---- Cross-cutting ---------------------------------------------------------

func TestPII_NoiseNotMatched(t *testing.T) {
	assertNothing(t, "550e8400-e29b-41d4-a716-446655440000")                             // UUID
	assertNothing(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") // sha256
	assertNothing(t, "deployed 2026-09-07T12:34:56Z build 1234567890 by ops")            // timestamp + order number
	assertNothing(t, "")
}

func TestPII_Redact(t *testing.T) {
	text := "Card 4111 1111 1111 1111 belongs to alice@example.com, thanks."
	d := NewPIIDetector(PIIOptions{})
	ms := d.Detect(text)
	got := Redact(text, ms)
	want := "Card [REDACTED:pan] belongs to [REDACTED:email], thanks."
	if got != want {
		t.Errorf("Redact = %q\nwant     %q\nmatches %v", got, want, classesOf(ms))
	}
}

func TestPII_ClassesFilter(t *testing.T) {
	text := "4111 1111 1111 1111 alice@example.com 123-45-6789 +14155552671"
	all := NewPIIDetector(PIIOptions{}).Detect(text)
	if len(all) != 4 {
		t.Fatalf("all classes: got %v", classesOf(all))
	}
	only := NewPIIDetector(PIIOptions{Classes: []PIIClass{ClassEmail, ClassPhone}}).Detect(text)
	if len(only) != 2 || only[0].RuleID != "email" || only[1].RuleID != "phone" {
		t.Errorf("filtered: got %v", classesOf(only))
	}
	none := NewPIIDetector(PIIOptions{Classes: []PIIClass{}}).Detect(text)
	if len(none) != 0 {
		t.Errorf("empty class list should detect nothing, got %v", classesOf(none))
	}
}

func TestPII_MatchesSortedAndNonOverlapping(t *testing.T) {
	text := strings.Repeat("x ", 5) + "+14155552671 then 5500 0000 0000 0004 and bob@example.org and GB82 WEST 1234 5698 7654 32"
	ms := NewPIIDetector(PIIOptions{}).Detect(text)
	for i := 1; i < len(ms); i++ {
		if ms[i].Offset < ms[i-1].Offset+ms[i-1].Length {
			t.Errorf("matches overlap or unsorted: %+v / %+v", ms[i-1], ms[i])
		}
	}
	if got := classesOf(ms); strings.Join(got, ",") != "phone,pan,email,iban" {
		t.Errorf("classes = %v", got)
	}
}

func TestPII_MaxSpan(t *testing.T) {
	d := NewPIIDetector(PIIOptions{})
	if d.MaxSpan() < 67 {
		t.Errorf("MaxSpan = %d, want at least a spaced 34-char IBAN", d.MaxSpan())
	}
	p := NewPIIDetector(PIIOptions{Classes: []PIIClass{ClassPhone}})
	if p.MaxSpan() != 16 {
		t.Errorf("phone-only MaxSpan = %d, want 16", p.MaxSpan())
	}
	if NewPIIDetector(PIIOptions{Classes: []PIIClass{}}).MaxSpan() != 0 {
		t.Error("no classes should give MaxSpan 0")
	}
	var _ SpanBound = d
}
