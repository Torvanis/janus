package httpapi

import (
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// totpCode produces the current code for secret with this edition's
// enrollment algorithm (SHA-1 default edition, SHA-256 FIPS edition).
func totpCode(secret string) string {
	c, err := totp.GenerateCodeCustom(secret, time.Now().UTC(), totp.ValidateOpts{
		Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otpAlgorithm(totpEnrollAlgorithm()),
	})
	if err != nil {
		panic(err)
	}
	return c
}
