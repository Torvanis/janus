GO ?= go
NPM ?= npm
VERSION ?= 2026.11.1
BUILD_DATE ?= 2026-11-07
GOTOOLCHAIN := go1.26.10
export GO VERSION BUILD_DATE GOTOOLCHAIN
.DEFAULT_GOAL := build
.PHONY: web-deps web web-test build build-fips vet test test-fips verify release run packaging-test
web-deps:
	cd web && $(NPM) ci --no-audit --no-fund
web: web-deps
	cd web && $(NPM) run build
web-test: web-deps
	cd web && $(NPM) run typecheck && $(NPM) test
build: web
	bash scripts/build.sh
# Janus FIPS edition: Go Cryptographic Module v1.0.0 (CMVP #5247), FIPS 140-3 mode.
build-fips: web
	EDITION=fips OUTPUT=bin/janus-fips bash scripts/build.sh
	./bin/janus-fips --fips-status
vet:
	@test -z "$$(gofmt -l cmd internal web/*.go)" || { printf '%s\n' 'Run gofmt before verification'; exit 1; }
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 -checks 'inherit,ST1016,ST1021' ./...
	GOFIPS140=v1.0.0 $(GO) vet -tags fips ./...
	GOFIPS140=v1.0.0 $(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 -tags fips -checks 'inherit,ST1016,ST1021' ./...
	@dead="$$($(GO) run golang.org/x/tools/cmd/deadcode@v0.50.0 -test ./cmd/... ./internal/...)" || exit 1; test -z "$$dead" || { printf '%s\n' "$$dead"; exit 1; }
test:
	CGO_ENABLED=1 $(GO) test -race -timeout 25m ./...
# FIPS edition tests in Go's strict FIPS-only mode.
test-fips:
	CGO_ENABLED=0 GOFIPS140=v1.0.0 GODEBUG=fips140=only $(GO) test -tags fips -count=1 ./...
verify:
	$(MAKE) web
	$(MAKE) vet test web-test packaging-test
release:
	python3 scripts/release.py
run:
	python3 scripts/run-local.py --binary ./bin/janus
packaging-test:
	python3 -m unittest discover -s scripts -p 'test_*.py' -v
