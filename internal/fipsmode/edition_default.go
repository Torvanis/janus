//go:build !fips

package fipsmode

// edition is fixed at compile time. Builds without the fips tag are the
// default edition: same code, same approved algorithms where the two
// editions share them, but no validated-module requirement and no FIPS-only
// restrictions.
const edition = EditionDefault
