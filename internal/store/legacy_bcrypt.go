//go:build !fips

package store

import "golang.org/x/crypto/bcrypt"

// legacyBcryptCompiled reports whether bcrypt verification of hashes written
// by releases before PBKDF2 is part of this build. The Janus FIPS edition
// leaves it out (legacy_bcrypt_fips.go): bcrypt is not a FIPS 140-3 approved
// algorithm.
const legacyBcryptCompiled = true

func bcryptMatches(hash, secret string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)) == nil
}
