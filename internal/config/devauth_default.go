//go:build !fips

package config

// DevAuthCompiled reports whether the JANUS_DEV_AUTH evaluation sign-in is
// part of this build. It is a constant so the compiler removes every
// dev-auth branch from the Janus FIPS edition (see devauth_fips.go).
const DevAuthCompiled = true
