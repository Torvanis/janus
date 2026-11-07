package httpapi

import (
	"net/url"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/torvanis/janus/internal/fipsmode"
	"github.com/torvanis/janus/internal/stigprofile"
	"github.com/torvanis/janus/internal/store"
)

// TOTP algorithm per edition and profile. The default edition enrolls
// HMAC-SHA-1, the RFC 6238 default every authenticator app understands. The
// FIPS edition, and any binary running the STIG profile, enrolls
// HMAC-SHA-256 (an approved HMAC) and refuses SHA-1 codes: the account's
// owner re-enrolls, or an administrator resets the second factor.

// totpEnrollAlgorithm is the algorithm new enrollments use in this edition.
func totpEnrollAlgorithm() string {
	if stigprofile.Allowed(fipsmode.FeatureTOTPSHA1) {
		return store.TOTPAlgorithmSHA1
	}
	return store.TOTPAlgorithmSHA256
}

func otpAlgorithm(algorithm string) otp.Algorithm {
	if algorithm == store.TOTPAlgorithmSHA256 {
		return otp.AlgorithmSHA256
	}
	return otp.AlgorithmSHA1
}

// totpUsable reports whether this edition can check codes for algorithm.
func totpUsable(algorithm string) bool {
	return algorithm == store.TOTPAlgorithmSHA256 || stigprofile.Allowed(fipsmode.FeatureTOTPSHA1)
}

// validateTOTP checks code for secret with the enrollment's own algorithm:
// 6 digits, 30-second period, one step of clock skew either way (the same
// window as totp.Validate). It never touches SHA-1 in the FIPS edition.
func validateTOTP(code, secret, algorithm string) bool {
	if !totpUsable(algorithm) {
		return false
	}
	ok, err := totp.ValidateCustom(code, secret, time.Now().UTC(), totp.ValidateOpts{
		Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otpAlgorithm(algorithm),
	})
	return err == nil && ok
}

// generateTOTPKey creates a new enrollment key for this edition.
func generateTOTPKey(account string) (*otp.Key, string, error) {
	alg := totpEnrollAlgorithm()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: account, Algorithm: otpAlgorithm(alg), Digits: otp.DigitsSix})
	return key, alg, err
}

// totpKeyURL rebuilds the otpauth:// URL for a staged secret.
func totpKeyURL(account, secret, algorithm string) string {
	return "otpauth://totp/" + url.PathEscape(totpIssuer+":"+account) + "?secret=" + secret + "&issuer=" + url.QueryEscape(totpIssuer) + "&algorithm=" + algorithm + "&digits=6&period=30"
}
