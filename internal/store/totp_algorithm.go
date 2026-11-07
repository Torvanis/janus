package store

import "strings"

// TOTP algorithm tagging.
//
// The encrypted totp_secret_enc column holds the base32 secret. Enrollments
// made before the FIPS edition existed are plain secrets and use HMAC-SHA-1
// (the RFC 6238 default every authenticator app supports). HMAC-SHA-256
// enrollments carry a "sha256:" prefix inside the encrypted value, so no
// schema change is needed and both editions read both forms: a SHA-256
// enrollment keeps working if a deployment moves back to the default
// edition, and a SHA-1 enrollment is recognized (and refused) by the FIPS
// edition instead of being misread.
const (
	TOTPAlgorithmSHA1   = "SHA1"
	TOTPAlgorithmSHA256 = "SHA256"
	totpSHA256Prefix    = "sha256:"
)

// encodeTOTPSecret tags secret with its algorithm for storage.
func encodeTOTPSecret(secret, algorithm string) string {
	if algorithm == TOTPAlgorithmSHA256 {
		return totpSHA256Prefix + secret
	}
	return secret
}

// decodeTOTPSecret splits a stored value into secret and algorithm.
func decodeTOTPSecret(stored string) (secret, algorithm string) {
	if s, ok := strings.CutPrefix(stored, totpSHA256Prefix); ok {
		return s, TOTPAlgorithmSHA256
	}
	return stored, TOTPAlgorithmSHA1
}
