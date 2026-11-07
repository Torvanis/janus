//go:build fips

package fipsmode

// edition is fixed at compile time. The fips tag marks the Janus FIPS
// edition; it must be combined with GOFIPS140=v1.0.0 so the binary links the
// CMVP-validated Go Cryptographic Module. CheckStartup refuses to run a fips
// build that was not.
const edition = EditionFIPS
