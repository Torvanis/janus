//go:build fips

package config

// DevAuthCompiled is false in the Janus FIPS edition: the JANUS_DEV_AUTH
// sign-in bypass is not compiled in, so no environment variable can turn it
// on, and Load refuses JANUS_DEV_AUTH=true outright.
const DevAuthCompiled = false
