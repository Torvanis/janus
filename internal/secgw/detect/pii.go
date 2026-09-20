package detect

import (
	"regexp"
	"strings"
)

// PIIClass names one category of personally identifiable information the
// PII detector recognises. The class name doubles as Match.RuleID.
type PIIClass string

const (
	// ClassPAN is a payment card primary account number (Luhn + IIN checked).
	ClassPAN PIIClass = "pan"
	// ClassIBAN is an international bank account number (ISO 7064 mod-97-10).
	ClassIBAN PIIClass = "iban"
	// ClassSSNUS is a United States Social Security Number (structural checks).
	ClassSSNUS PIIClass = "ssn_us"
	// ClassNPI is a US National Provider Identifier (Luhn over "80840"+digits).
	ClassNPI PIIClass = "npi"
	// ClassEmail is an RFC-ish email address with a real-looking TLD.
	ClassEmail PIIClass = "email"
	// ClassPhone is an E.164 or NANP-formatted telephone number.
	ClassPhone PIIClass = "phone"
)

// allPIIClasses lists every class in evaluation priority order. When two
// classes claim overlapping spans, the class that appears earlier here wins
// (an IBAN's digit groups must not be re-reported as a PAN, a PAN's groups
// must not be re-reported as a phone number, and so on).
var allPIIClasses = []PIIClass{ClassIBAN, ClassPAN, ClassSSNUS, ClassNPI, ClassEmail, ClassPhone}

// PIIOptions configures NewPIIDetector.
type PIIOptions struct {
	// Classes restricts detection to the listed classes. nil means all.
	Classes []PIIClass
	// AllowBareSSN additionally reports un-hyphenated nine-digit runs that
	// satisfy the SSN structural rules. This is documented as high
	// false-positive: nine-digit runs are also routing numbers, order IDs
	// and ZIP+4 codes. Off by default.
	AllowBareSSN bool
}

// PIIDetector finds PII in text. It is safe for concurrent use.
type PIIDetector struct {
	enabled      map[PIIClass]bool
	allowBareSSN bool
}

// NewPIIDetector builds a detector for the requested classes.
func NewPIIDetector(opts PIIOptions) *PIIDetector {
	d := &PIIDetector{enabled: map[PIIClass]bool{}, allowBareSSN: opts.AllowBareSSN}
	if opts.Classes == nil {
		for _, c := range allPIIClasses {
			d.enabled[c] = true
		}
	} else {
		for _, c := range opts.Classes {
			d.enabled[c] = true
		}
	}
	return d
}

// Compiled patterns. All RE2: linear time, no backtracking.
var (
	// 13–19 digits, optionally separated by single spaces or hyphens.
	// The final atom is a bare digit so the span never ends on a separator.
	rePAN = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

	// Country code, two check digits, then alphanumerics with optional single
	// spaces. The span is trimmed to the country's length in Go afterwards.
	reIBAN = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}`)

	reSSNHyphen = regexp.MustCompile(`\b(\d{3})-(\d{2})-(\d{4})\b`)
	reSSNBare   = regexp.MustCompile(`\b\d{9}\b`)

	reNPI = regexp.MustCompile(`\b\d{10}\b`)

	// Local part and labels are bounded so MaxSpan is finite.
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]{1,63}(?:\.[A-Za-z0-9-]{1,63}){0,8}\.[A-Za-z]{2,24}`)

	// E.164: "+" then 8–15 digits not starting with 0.
	rePhoneE164 = regexp.MustCompile(`\+[1-9]\d{7,14}\b`)
	// NANP: (NXX) or NXX, optional separator, NXX, mandatory separator, XXXX.
	// The mandatory separator between exchange and line is what keeps a bare
	// ten-digit run (order numbers, NPIs) out of this class.
	rePhoneNANP = regexp.MustCompile(`(?:\(\d{3}\)|\b\d{3})[-. ]?\d{3}[-. ]\d{4}\b`)
)

// maxSpans records the longest span each class can produce.
const (
	maxSpanIBAN  = 34 + 33 // 34 alphanumerics with a space between each
	maxSpanPAN   = 19 + 18 // 19 digits with a separator between each
	maxSpanEmail = 64 + 1 + 63 + 8*64 + 1 + 24
	maxSpanPhone = 16
	maxSpanNPI   = 10
	maxSpanSSN   = 11
)

// MaxSpan reports the longest Length any enabled class can produce.
func (d *PIIDetector) MaxSpan() int {
	max := 0
	set := func(c PIIClass, n int) {
		if d.enabled[c] && n > max {
			max = n
		}
	}
	set(ClassIBAN, maxSpanIBAN)
	set(ClassPAN, maxSpanPAN)
	set(ClassEmail, maxSpanEmail)
	set(ClassPhone, maxSpanPhone)
	set(ClassNPI, maxSpanNPI)
	set(ClassSSNUS, maxSpanSSN)
	return max
}

// Detect returns every PII match in text, ordered by offset.
func (d *PIIDetector) Detect(text string) []Match {
	var out []Match
	add := func(class PIIClass, sev Severity, off, length int) {
		// Suppress overlaps with higher-priority classes already accepted.
		for _, m := range out {
			if off < m.Offset+m.Length && m.Offset < off+length {
				return
			}
		}
		out = append(out, Match{
			Kind:        KindPII,
			RuleID:      string(class),
			Severity:    sev,
			Offset:      off,
			Length:      length,
			Text:        text[off : off+length],
			Replacement: "[REDACTED:" + string(class) + "]",
		})
	}
	for _, class := range allPIIClasses {
		if !d.enabled[class] {
			continue
		}
		switch class {
		case ClassIBAN:
			d.detectIBAN(text, add)
		case ClassPAN:
			d.detectPAN(text, add)
		case ClassSSNUS:
			d.detectSSN(text, add)
		case ClassNPI:
			d.detectNPI(text, add)
		case ClassEmail:
			d.detectEmail(text, add)
		case ClassPhone:
			d.detectPhone(text, add)
		}
	}
	sortByOffset(out)
	return out
}

type addFunc func(class PIIClass, sev Severity, off, length int)

// ---- PAN -------------------------------------------------------------------

func (d *PIIDetector) detectPAN(text string, add addFunc) {
	for _, loc := range rePAN.FindAllStringIndex(text, -1) {
		span := text[loc[0]:loc[1]]
		digits := stripSeparators(span)
		if len(digits) < 13 || len(digits) > 19 {
			continue
		}
		if !plausibleIIN(digits) || !Luhn(digits) {
			continue
		}
		add(ClassPAN, SeverityHigh, loc[0], loc[1]-loc[0])
	}
}

func stripSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// plausibleIIN reports whether digits begin with a known issuer prefix:
// Visa 4; Mastercard 51–55 and 2221–2720; Amex 34/37; Discover 6011, 65,
// 644–649; JCB 35; Diners 30/36/38.
func plausibleIIN(digits string) bool {
	if len(digits) < 4 {
		return false
	}
	p2 := int(digits[0]-'0')*10 + int(digits[1]-'0')
	p3 := p2*10 + int(digits[2]-'0')
	p4 := p3*10 + int(digits[3]-'0')
	switch {
	case digits[0] == '4':
		return true
	case p2 >= 51 && p2 <= 55:
		return true
	case p4 >= 2221 && p4 <= 2720:
		return true
	case p2 == 34 || p2 == 37:
		return true
	case p4 == 6011 || p2 == 65 || (p3 >= 644 && p3 <= 649):
		return true
	case p2 == 35:
		return true
	case p2 == 30 || p2 == 36 || p2 == 38:
		return true
	}
	return false
}

// Luhn reports whether digits (ASCII digits only, at least two) satisfy the
// Luhn mod-10 check. Any non-digit byte makes it return false.
func Luhn(digits string) bool {
	if len(digits) < 2 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		n := int(c - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// ---- IBAN ------------------------------------------------------------------

// ibanLengths is the total IBAN length (including country code and check
// digits) per ISO 3166 country code.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16,
	"BG": 22, "BH": 22, "BR": 29, "CH": 21, "CR": 22, "CY": 28, "CZ": 24,
	"DE": 22, "DK": 18, "DO": 28, "EE": 20, "ES": 24, "FI": 18, "FO": 18,
	"FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28,
	"HR": 21, "HU": 28, "IE": 22, "IL": 23, "IS": 26, "IT": 27, "JO": 30,
	"KW": 30, "KZ": 20, "LB": 28, "LI": 21, "LT": 20, "LU": 20, "LV": 21,
	"MC": 27, "MD": 24, "ME": 22, "MK": 19, "MR": 27, "MT": 31, "MU": 30,
	"NL": 18, "NO": 15, "PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29,
	"RO": 24, "RS": 22, "SA": 24, "SE": 24, "SI": 19, "SK": 24, "SM": 27,
	"TN": 24, "TR": 26, "UA": 29, "VG": 24, "XK": 20,
}

func (d *PIIDetector) detectIBAN(text string, add addFunc) {
	for _, loc := range reIBAN.FindAllStringIndex(text, -1) {
		span := text[loc[0]:loc[1]]
		want, ok := ibanLengths[span[:2]]
		if !ok {
			continue
		}
		// Walk the span to find where the want-th alphanumeric ends.
		count, end := 0, -1
		for i := 0; i < len(span); i++ {
			if span[i] != ' ' {
				count++
				if count == want {
					end = i + 1
					break
				}
			}
		}
		if end < 0 {
			continue // shorter than the country requires
		}
		// The character following the trimmed span must not continue the
		// alphanumeric run (a longer number that merely starts like an IBAN).
		if next := loc[0] + end; next < len(text) && isUpperAlnum(text[next]) {
			continue
		}
		candidate := span[:end]
		if !IBANValid(candidate) {
			continue
		}
		add(ClassIBAN, SeverityHigh, loc[0], end)
	}
}

func isUpperAlnum(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// IBANValid reports whether s is a structurally valid IBAN: a known country
// code, the country's exact length, and an ISO 7064 mod-97-10 remainder of 1.
// Spaces are ignored; letters must be upper-case.
func IBANValid(s string) bool {
	compact := strings.ReplaceAll(s, " ", "")
	if len(compact) < 5 {
		return false
	}
	want, ok := ibanLengths[compact[:2]]
	if !ok || len(compact) != want {
		return false
	}
	for i := 0; i < len(compact); i++ {
		if !isUpperAlnum(compact[i]) {
			return false
		}
	}
	if !isUpperAlnum(compact[2]) || compact[2] < '0' || compact[2] > '9' ||
		compact[3] < '0' || compact[3] > '9' {
		return false
	}
	// Move the first four characters to the end, then take a running
	// remainder mod 97 with letters expanded to 10..35. No big integers.
	rearranged := compact[4:] + compact[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		if c >= '0' && c <= '9' {
			rem = (rem*10 + int(c-'0')) % 97
		} else {
			v := int(c-'A') + 10
			rem = (rem*100 + v) % 97
		}
	}
	return rem == 1
}

// ---- SSN -------------------------------------------------------------------

func (d *PIIDetector) detectSSN(text string, add addFunc) {
	for _, loc := range reSSNHyphen.FindAllStringIndex(text, -1) {
		span := text[loc[0]:loc[1]]
		if ssnStructureOK(span[0:3], span[4:6], span[7:11]) {
			add(ClassSSNUS, SeverityHigh, loc[0], loc[1]-loc[0])
		}
	}
	if !d.allowBareSSN {
		return
	}
	for _, loc := range reSSNBare.FindAllStringIndex(text, -1) {
		span := text[loc[0]:loc[1]]
		if ssnStructureOK(span[0:3], span[3:5], span[5:9]) {
			add(ClassSSNUS, SeverityHigh, loc[0], loc[1]-loc[0])
		}
	}
}

// ssnStructureOK applies the SSA issuance rules: area not 000, 666 or
// 900–999; group not 00; serial not 0000.
func ssnStructureOK(area, group, serial string) bool {
	if area == "000" || area == "666" || area[0] == '9' {
		return false
	}
	if group == "00" || serial == "0000" {
		return false
	}
	return true
}

// ---- NPI -------------------------------------------------------------------

// detectNPI reports ten-digit runs that pass Luhn once prefixed with the
// ISO 7812 health-industry prefix 80840. A bare ten-digit run also looks
// like an unformatted NANP phone number, so the run is skipped when the
// surrounding bytes suggest telephone context: a preceding '+', '-', '(' or
// ')' or a following '-'. Runs the phone detector itself accepts (they
// contain separators) can never reach here because \b\d{10}\b requires a
// contiguous run.
func (d *PIIDetector) detectNPI(text string, add addFunc) {
	for _, loc := range reNPI.FindAllStringIndex(text, -1) {
		if loc[0] > 0 {
			switch text[loc[0]-1] {
			case '+', '-', '(', ')':
				continue
			}
		}
		if loc[1] < len(text) && text[loc[1]] == '-' {
			continue
		}
		if !Luhn("80840" + text[loc[0]:loc[1]]) {
			continue
		}
		add(ClassNPI, SeverityMedium, loc[0], loc[1]-loc[0])
	}
}

// ---- Email -----------------------------------------------------------------

func (d *PIIDetector) detectEmail(text string, add addFunc) {
	for _, loc := range reEmail.FindAllStringIndex(text, -1) {
		span := text[loc[0]:loc[1]]
		// Reject a local part beginning or ending with a dot and a domain
		// label that begins or ends with a hyphen.
		at := strings.IndexByte(span, '@')
		local, domain := span[:at], span[at+1:]
		if local[0] == '.' || local[len(local)-1] == '.' || strings.Contains(local, "..") {
			continue
		}
		bad := false
		for _, label := range strings.Split(domain, ".") {
			if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		add(ClassEmail, SeverityLow, loc[0], loc[1]-loc[0])
	}
}

// ---- Phone -----------------------------------------------------------------

func (d *PIIDetector) detectPhone(text string, add addFunc) {
	for _, loc := range rePhoneE164.FindAllStringIndex(text, -1) {
		// "+" must not be glued to a preceding alphanumeric (e.g. "a+1234...").
		if loc[0] > 0 && isWordByte(text[loc[0]-1]) {
			continue
		}
		add(ClassPhone, SeverityLow, loc[0], loc[1]-loc[0])
	}
	for _, loc := range rePhoneNANP.FindAllStringIndex(text, -1) {
		// A "(" form has no \b guard on the left; refuse a preceding digit.
		if loc[0] > 0 && text[loc[0]] == '(' && isDigitByte(text[loc[0]-1]) {
			continue
		}
		add(ClassPhone, SeverityLow, loc[0], loc[1]-loc[0])
	}
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isWordByte(c byte) bool {
	return isDigitByte(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
