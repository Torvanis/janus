//go:build !fips

package reporting

// PDFAvailable reports whether report export as PDF is compiled in. It is
// false in the Janus FIPS edition: the PDF library hashes embedded fonts with
// SHA-1 and implements MD5/RC4 PDF encryption, neither of which is a FIPS
// 140-3 approved algorithm.
const PDFAvailable = true
