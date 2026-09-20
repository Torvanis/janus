module github.com/torvanis/janus

go 1.26.3

// The `go` directive stays at the version installed in the pinned
// (GOTOOLCHAIN=local) runner pods so local test runs keep working; the
// `toolchain` directive lifts environments with GOTOOLCHAIN=auto (GitHub CI,
// release builds) to a patch release that fixes all stdlib vulnerabilities
// currently flagged by govulncheck. Under GOTOOLCHAIN=local the toolchain
// line is ignored, so it cannot brick the pinned pods.
toolchain go1.26.7

require (
	github.com/go-chi/chi/v5 v5.3.1
	github.com/go-ldap/ldap/v3 v3.4.14
	github.com/go-pdf/fpdf v0.9.0
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/jackc/pgx/v5 v5.9.2
	github.com/pquerna/otp v1.5.0
	github.com/prometheus/client_golang v1.20.5
	github.com/prometheus/client_model v0.6.1
	golang.org/x/crypto v0.57.0
	golang.org/x/image v0.39.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.34.1
)

require (
	github.com/Azure/go-ntlmssp v0.1.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/boombuler/barcode v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-asn1-ber/asn1-ber v1.5.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)
