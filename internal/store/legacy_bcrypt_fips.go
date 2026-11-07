//go:build fips

package store

// legacyBcryptCompiled is false in the Janus FIPS edition: bcrypt is not an
// approved algorithm and is not linked. Accounts still on a bcrypt hash need
// an administrator password reset; legacy bcrypt recovery codes are not
// accepted (new codes are HMAC-SHA-256).
const legacyBcryptCompiled = false

func bcryptMatches(string, string) bool { return false }
