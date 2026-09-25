GO ?= go
NPM ?= npm
VERSION ?= 2026.9.4
BUILD_DATE ?= 2026-09-25
GOTOOLCHAIN := go1.26.7
export GO VERSION BUILD_DATE GOTOOLCHAIN
.DEFAULT_GOAL := build
.PHONY: web-deps web web-test build vet test verify release run packaging-test
web-deps:
	cd web && $(NPM) ci --no-audit --no-fund
web: web-deps
	cd web && $(NPM) run build
web-test: web-deps
	cd web && $(NPM) run typecheck && $(NPM) test
build: web
	bash scripts/build.sh
vet:
	@test -z "$$(gofmt -l cmd internal web/*.go)" || { printf '%s\n' 'Run gofmt before verification'; exit 1; }
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 -checks 'inherit,ST1016,ST1021' ./...
	@dead="$$($(GO) run golang.org/x/tools/cmd/deadcode@v0.50.0 -test ./cmd/... ./internal/...)" || exit 1; test -z "$$dead" || { printf '%s\n' "$$dead"; exit 1; }
test:
	CGO_ENABLED=1 $(GO) test -race ./...
verify:
	$(MAKE) web
	$(MAKE) vet test web-test packaging-test
release:
	python3 scripts/release.py
run:
	python3 scripts/run-local.py --binary ./bin/janus
packaging-test:
	python3 -m unittest discover -s scripts -p 'test_*.py' -v
