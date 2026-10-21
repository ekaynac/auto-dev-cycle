# SPDX-License-Identifier: Apache-2.0
#
# The only entry point for humans and CI (docs/design/m0/01 "Build and CI").
# Targets that are not built yet are added by the PR that builds them.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

GOROOT_PIN := $(shell scripts/bootstrap-go.sh --print-goroot)
GO_PIN     := $(shell scripts/bootstrap-go.sh --print-version)
ifeq ($(strip $(GOROOT_PIN)),)
$(error scripts/bootstrap-go.sh --print-goroot failed; see its message above)
endif
ifeq ($(strip $(GO_PIN)),)
$(error scripts/bootstrap-go.sh --print-version failed; see its message above)
endif
GO         := $(GOROOT_PIN)/bin/go
# Never let `go` fetch a different toolchain: go.mod's toolchain line is the pin.
export GOTOOLCHAIN := local
export CGO_ENABLED := 0
# The caller's Go settings never leak into builds: not persisted ones (go env
# -w), not the environment, not a make command line (override). A
# -tags=adcmutation would put live mutation sites into a release binary.
override export GOENV := off
override export GOFLAGS :=
override export GOEXPERIMENT :=
# Every release binary is checked for build tags, decoded from its recorded
# build settings, whatever set them.
CHECK_RELEASE := $(GO) run ./tools/releasecheck

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

BUILDINFO  := github.com/ekaynac/auto-dev-cycle/internal/buildinfo
LDFLAGS    := -s -w -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT)
GOBUILD    := $(GO) build -trimpath -ldflags '$(LDFLAGS)'

BIN_DIR    := bin
DIST_DIR   := dist
BINARIES   := adcd adc adc-harness adc-agent
# adcd, adc and adc-harness run on the Linux controller; adc-agent is cross-platform (plan §2.12, §2.14).
SERVER_PLATFORMS := linux/amd64 linux/arm64
AGENT_PLATFORMS  := $(SERVER_PLATFORMS) darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help toolchain go-check ts-deps schema-docs gen gen-check inventory-count vocab-check build dist install-check installed-cycle-stub fixtures-contract release-guard lint test test-sandbox-selftest fixtures-check ci clean

SHELL_SCRIPTS = scripts/*.sh scripts/lib/*.sh fixtures/t0/*/check.sh
T0_CHECKS     := $(wildcard fixtures/t0/*/check.sh)

help: ## List targets
	@grep -hE '^[a-z][a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{ printf "  %-12s %s\n", $$1, $$2 }'

toolchain: ## Install the pinned Go toolchain per user (~/.local/go)
	@scripts/bootstrap-go.sh

go-check:
	@have="$$(head -n1 $(GOROOT_PIN)/VERSION 2>/dev/null || true)"; \
	  if [[ "$$have" != "$(GO_PIN)" ]]; then \
	    echo "Go $(GO_PIN) required at $(GOROOT_PIN) (found '$${have:-nothing}'); run 'make toolchain'" >&2; exit 1; \
	  fi

build: go-check ## Build the binaries for this host into bin/
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do $(GOBUILD) -o $(BIN_DIR)/$$b ./cmd/$$b; done
	@$(CHECK_RELEASE) $(addprefix $(BIN_DIR)/,$(BINARIES))
	@$(BIN_DIR)/adc version

dist: go-check ts-deps ## Cross-build release binaries into dist/<os>-<arch>/; for the server platforms, lib/adc gets the pinned Node, the fake adapter and py-mini (15 S2, D6)
	@rm -rf $(DIST_DIR)
	@for p in $(SERVER_PLATFORMS); do \
	  for b in adcd adc adc-harness; do \
	    GOOS=$${p%/*} GOARCH=$${p#*/} $(GOBUILD) -o $(DIST_DIR)/$${p%/*}-$${p#*/}/$$b ./cmd/$$b; \
	  done; \
	done
	@for p in $(AGENT_PLATFORMS); do \
	  ext=""; [[ $${p%/*} == windows ]] && ext=.exe; \
	  GOOS=$${p%/*} GOARCH=$${p#*/} $(GOBUILD) -o $(DIST_DIR)/$${p%/*}-$${p#*/}/adc-agent$$ext ./cmd/adc-agent; \
	done
	@$(CHECK_RELEASE) $$(find $(DIST_DIR) -mindepth 2 -maxdepth 2 -type f | sort)
	@scripts/dist-lib.sh $(DIST_DIR) $(SERVER_PLATFORMS)
	@find $(DIST_DIR) -mindepth 2 -maxdepth 2 | sort

# INSTALL_PREFIX installs the dist build into $$tmp/prefix, a fresh temporary directory removed on exit.
INSTALL_PREFIX = tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
  scripts/install-check.sh --expect "$$($(BIN_DIR)/adc version --json | sed -n 's/.*"schema_set":"\([^"]*\)".*/\1/p')" "$$tmp/prefix"

install-check: dist build ## Install the dist build into an empty prefix, check what the installed binaries report, run their contract fixtures and the installed smoke in isolation (00 exit check, 14 PR 12)
	@$(INSTALL_PREFIX) \
	  && scripts/installed-fixtures.sh "$$tmp/prefix" \
	  && scripts/installed-smoke.sh "$$tmp/prefix"

installed-cycle-stub: dist build ## Install the dist build into an empty prefix and run the installed cycle harness's stub cases from outside the checkout (P24; preflight until P22a lands)
	@$(INSTALL_PREFIX) \
	  && mkdir "$$tmp/cwd" && (cd "$$tmp/cwd" && "$$tmp/prefix/lib/adc/scripts/installed-cycle.sh" "$$tmp/prefix" --routes stub --project go-mini --case preflight) \
	  && for c in $$(cd fixtures/m1/cases && ls -d hostile-*); do \
	    (cd "$$tmp/cwd" && "$$tmp/prefix/lib/adc/scripts/installed-cycle.sh" "$$tmp/prefix" --routes stub --project go-mini --case "$$c") >"$$tmp/$$c.out" 2>&1 && rc=0 || rc=$$?; \
	    if [ "$$rc" -ne 2 ] || ! grep -q "could_not_run: $$c needs project and task create" "$$tmp/$$c.out"; then \
	      echo "installed-cycle-stub: $$c: its setup failed (exit $$rc):" >&2; tail -n 5 "$$tmp/$$c.out" >&2; exit 1; fi; \
	    echo "installed-cycle-stub: $$c: setup checked; the run needs P22a"; done

GEN_GO    := gen/go/types
VOCAB_GEN := internal/vocab/vocab.gen.go
TS_GEN    := gen/ts
TS_GEN_SCRIPT := adapters/contract/scripts/gen.mjs
API_DOC   := schemas/openapi.yaml
SCHEMA_DOCS := docs/reference/schemas
PARITY_DOC := docs/reference/action-matrix.md
INVENTORY := docs/design/m1/01-invariant-inventory.md

schema-docs: go-check ## Regenerate docs/reference/schemas from schemas/ (also part of make gen)
	@$(GO) run ./tools/schemadoc $(SCHEMA_DOCS)

ts-deps: ## Install the adapters workspace dependencies (pnpm, from the lockfile)
	@# corepack picks the pnpm version from the current directory (adapters/package.json), not from --dir.
	@cd adapters && pnpm install --frozen-lockfile

gen: go-check ts-deps ## Regenerate from schemas/: Go types (gen/go/types), vocabulary constants, openapi.yaml, the action matrix, TypeScript types and validators (gen/ts)
	@$(GO) run ./tools/schemagen $(GEN_GO)
	@$(GO) run ./tools/vocabgen $(VOCAB_GEN)
	@$(GO) run ./tools/apigen $(API_DOC)
	@$(GO) run ./tools/paritygen $(PARITY_DOC)
	@$(GO) run ./tools/schemadoc $(SCHEMA_DOCS)
	@node $(TS_GEN_SCRIPT) $(TS_GEN)

inventory-count: go-check ## Print the invariant inventory's rows by status (counted from its tables, never kept in it)
	@$(GO) run ./tools/inventorycount $(INVENTORY)

gen-check: go-check ts-deps ## Fail if the committed generated code differs from a fresh generation
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	  $(GO) run ./tools/schemagen "$$tmp/types" && diff -r -x '*_test.go' "$$tmp/types" $(GEN_GO) \
	  && $(GO) run ./tools/vocabgen "$$tmp/vocab.gen.go" && diff "$$tmp/vocab.gen.go" $(VOCAB_GEN) \
	  && $(GO) run ./tools/apigen "$$tmp/openapi.yaml" && diff "$$tmp/openapi.yaml" $(API_DOC) \
	  && $(GO) run ./tools/paritygen "$$tmp/action-matrix.md" && diff "$$tmp/action-matrix.md" $(PARITY_DOC) \
	  && $(GO) run ./tools/schemadoc "$$tmp/schema-docs" && diff -r "$$tmp/schema-docs" $(SCHEMA_DOCS) \
	  && node $(TS_GEN_SCRIPT) "$$tmp/ts" && diff -r -x node_modules "$$tmp/ts" $(TS_GEN) \
	  || { echo "gen-check: generated code is stale; run 'make gen'" >&2; exit 1; }

lint: go-check ## gofmt, go vet, go mod tidy check, SPDX headers, shell syntax (+ shellcheck where installed)
	@unformatted="$$($(GOROOT_PIN)/bin/gofmt -l cmd internal schemas test tools gen fixtures/contract)"; \
	  if [[ -n "$$unformatted" ]]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	@$(GO) vet ./...
	@$(GO) vet -tags corpus ./internal/accept/ # the corpus runner, compiled, never run (Opus #296 L6)
	@$(GO) mod tidy -diff
	@for f in $(SHELL_SCRIPTS); do bash -n "$$f"; done
	@scripts/check-spdx.sh
	@$(GO) vet -tags qualify ./internal/qualify/ # the live qualification run, compiled, never run (P31a)
	@if command -v shellcheck >/dev/null; then shellcheck $(SHELL_SCRIPTS); \
	  elif [[ -n "$${CI:-}" ]]; then echo "shellcheck is required in CI" >&2; exit 1; \
	  else echo "lint: shellcheck not installed; skipped locally (required in CI)"; fi

fixtures-check: ## Prove every T0 fixture task well-formed (needs docker, bubblewrap and network for dependency recipes)
	@[[ -n "$(T0_CHECKS)" ]] || { echo "fixtures-check: no fixtures/t0/*/check.sh found" >&2; exit 1; }
	@for c in $(T0_CHECKS); do echo "== $$c"; $$c || exit 1; done

# The containment tests inspect every process visible in the sandbox, so they
# run alone: other packages' go tooling would otherwise appear beside them.
# The second run discovers packages inside the sandbox (its Go environment),
# excludes exactly the containment package, and refuses a failed or empty
# discovery rather than testing less.
test: go-check ts-deps ## Run all tests inside the test sandbox (refuses to run unsandboxed)
	@scripts/test-sandbox.sh -- go test -count=1 ./test/sandbox/
	@scripts/test-sandbox.sh -- bash -c 'set -euo pipefail; \
	  mod=$$(go list -m); listed=$$(go list ./...); others=(); \
	  while IFS= read -r p; do [[ "$$p" == "$$mod/test/sandbox" ]] || others+=("$$p"); done <<<"$$listed"; \
	  (( $${#others[@]} > 0 )) || { echo "make test: no packages besides test/sandbox" >&2; exit 1; }; \
	  go test -count=1 "$${others[@]}"'
	@scripts/test-sandbox.sh -- bash -c 'cd adapters/contract && node --test "test/*.test.mjs" "test/*.test.ts" && ./node_modules/.bin/tsc -p tsconfig.json'
	@scripts/test-sandbox.sh -- bash -c 'cd adapters/fake && node --test "test/*.test.ts" && ./node_modules/.bin/tsc -p tsconfig.json'
	@scripts/test-sandbox.sh -- bash -c 'cd adapters/conformance && node --test "test/*.test.ts" && ./node_modules/.bin/tsc -p tsconfig.json'
	@scripts/test-sandbox.sh -- bash -c 'cd adapters/http-chat && node --test "test/*.test.ts" && ./node_modules/.bin/tsc -p tsconfig.json'
	@scripts/test-sandbox.sh -- bash -c 'cd fixtures/m1/stub-provider && node --test "test/*.test.mjs"'

# The contract fixtures (00 "M0 exit check"): green through the real
# components with the ordinary adc, and each red under its named mutation
# with a separate build that alone can switch mutations on; both runs in the
# test sandbox. The mutation build never goes into dist.
fixtures-contract: build ## Run the M0 contract fixtures, and check each fixture's mutation turns it red
	@scripts/test-sandbox.sh -- $(BIN_DIR)/adc dev fixtures --contract
	@$(GOBUILD) -tags adcmutation -o $(BIN_DIR)/adc-mutation ./cmd/adc
	@scripts/test-sandbox.sh -- go test -count=1 -tags adcmutation ./internal/fixtures
	@$(BIN_DIR)/adc dev fixtures --contract --list | while read -r name mut; do \
	  rc=0; scripts/test-sandbox.sh -- env ADC_MUTATION="$$mut" $(BIN_DIR)/adc-mutation dev fixtures --contract --only "$$name" >/dev/null || rc=$$?; \
	  [[ $$rc -eq 1 ]] || { echo "fixtures-contract: mutation $$mut gave exit $$rc on $$name, want 1 (red)" >&2; exit 1; }; \
	  echo "fixtures-contract: $$mut turns $$name red"; \
	done

# The release-build guard's regression: builds under a hostile persisted Go
# configuration (GOFLAGS=-tags=adcmutation in a go env file) and under a
# command-line GOFLAGS whose tag list Go records quoted still yield binaries
# without tags; make build and make dist (for this host) each refuse what
# they built with a tag injected through GOBUILD, also with the build
# information erased to hide it; and the guard refuses the mutation build
# and a binary built directly with the quoted tag list.
release-guard: fixtures-contract ## Prove release builds ignore a persisted mutation tag
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	  printf 'GOFLAGS=-tags=adcmutation\n' >"$$tmp/goenv"; \
	  GOENV="$$tmp/goenv" $(MAKE) --no-print-directory build BIN_DIR="$$tmp/bin" >/dev/null; \
	  $(CHECK_RELEASE) $(addprefix "$$tmp/bin/,$(addsuffix ",$(BINARIES))); \
	  $(MAKE) --no-print-directory build BIN_DIR="$$tmp/cli" 'GOFLAGS=-tags=adcmutation,x"' >/dev/null; \
	  $(CHECK_RELEASE) $(addprefix "$$tmp/cli/,$(addsuffix ",$(BINARIES))); \
	  host="$$($(GO) env GOOS)/$$($(GO) env GOARCH)"; \
	  for gb in "$(GO) build -trimpath -tags adcmutation" "$(GO) build -trimpath -tags adcmutation -ldflags=-X=runtime.modinfo="; do \
	    if $(MAKE) --no-print-directory build BIN_DIR="$$tmp/gobuild" GOBUILD="$$gb" >/dev/null 2>&1; then \
	      echo "release-guard: make build accepted binaries built with: $$gb" >&2; exit 1; \
	    fi; \
	    if $(MAKE) --no-print-directory dist DIST_DIR="$$tmp/godist" SERVER_PLATFORMS="$$host" AGENT_PLATFORMS="$$host" GOBUILD="$$gb" >/dev/null 2>&1; then \
	      echo "release-guard: make dist accepted binaries built with: $$gb" >&2; exit 1; \
	    fi; \
	  done; \
	  $(GO) build -tags 'adcmutation,x"' -o "$$tmp/quoted" ./cmd/adc; \
	  for f in $(BIN_DIR)/adc-mutation "$$tmp/quoted"; do \
	    if $(CHECK_RELEASE) "$$f" 2>/dev/null; then echo "release-guard: the guard accepted $$f" >&2; exit 1; fi; \
	  done; \
	  echo "release-guard: hostile go env and command-line GOFLAGS left no tags; the guard refuses tagged builds, quoted ones included"

test-sandbox-selftest: go-check ## Prove the test sandbox contains writes, credentials and network
	@scripts/test-sandbox-selftest.sh

vocab-check: build ## Registries cross-check; reasons only through generated constants
	@# The check type-checks the module through go list, so the pinned go is put first on PATH.
	@PATH="$(GOROOT_PIN)/bin:$$PATH" $(BIN_DIR)/adc dev vocab check --root .

ci: lint gen-check build vocab-check test fixtures-contract release-guard test-sandbox-selftest dist install-check installed-cycle-stub ## Everything CI runs

clean: ## Remove build outputs
	@rm -rf $(BIN_DIR) $(DIST_DIR)
