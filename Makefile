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
# The caller's Go settings never leak into builds.
export GOFLAGS :=
export GOEXPERIMENT :=

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

BUILDINFO  := github.com/ekaynac/auto-dev-cycle/internal/buildinfo
LDFLAGS    := -s -w -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT)
GOBUILD    := $(GO) build -trimpath -ldflags '$(LDFLAGS)'

BIN_DIR    := bin
DIST_DIR   := dist
BINARIES   := adcd adc adc-agent
# adcd and adc run on the Linux controller; adc-agent is cross-platform (plan §2.12, §2.14).
SERVER_PLATFORMS := linux/amd64 linux/arm64
AGENT_PLATFORMS  := $(SERVER_PLATFORMS) darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: help toolchain go-check gen gen-check build dist lint test test-sandbox-selftest fixtures-check ci clean

SHELL_SCRIPTS = scripts/*.sh fixtures/t0/*/check.sh
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
	@$(BIN_DIR)/adc version

dist: go-check ## Cross-build release binaries into dist/<os>-<arch>/
	@rm -rf $(DIST_DIR)
	@for p in $(SERVER_PLATFORMS); do \
	  for b in adcd adc; do \
	    GOOS=$${p%/*} GOARCH=$${p#*/} $(GOBUILD) -o $(DIST_DIR)/$${p%/*}-$${p#*/}/$$b ./cmd/$$b; \
	  done; \
	done
	@for p in $(AGENT_PLATFORMS); do \
	  ext=""; [[ $${p%/*} == windows ]] && ext=.exe; \
	  GOOS=$${p%/*} GOARCH=$${p#*/} $(GOBUILD) -o $(DIST_DIR)/$${p%/*}-$${p#*/}/adc-agent$$ext ./cmd/adc-agent; \
	done
	@find $(DIST_DIR) -type f | sort

GEN_GO := gen/go/types

gen: go-check ## Regenerate the Go types (gen/go/types) from schemas/
	@$(GO) run ./tools/schemagen $(GEN_GO)

gen-check: go-check ## Fail if the committed generated code differs from a fresh generation
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	  $(GO) run ./tools/schemagen "$$tmp/types" && diff -r -x '*_test.go' "$$tmp/types" $(GEN_GO) \
	  || { echo "gen-check: generated code is stale; run 'make gen'" >&2; exit 1; }

lint: go-check ## gofmt, go vet, go mod tidy check, SPDX headers, shell syntax (+ shellcheck where installed)
	@unformatted="$$($(GOROOT_PIN)/bin/gofmt -l cmd internal schemas test tools gen)"; \
	  if [[ -n "$$unformatted" ]]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	@$(GO) vet ./...
	@$(GO) mod tidy -diff
	@for f in $(SHELL_SCRIPTS); do bash -n "$$f"; done
	@scripts/check-spdx.sh
	@if command -v shellcheck >/dev/null; then shellcheck $(SHELL_SCRIPTS); \
	  elif [[ -n "$${CI:-}" ]]; then echo "shellcheck is required in CI" >&2; exit 1; \
	  else echo "lint: shellcheck not installed; skipped locally (required in CI)"; fi

fixtures-check: ## Prove every T0 fixture task well-formed (needs docker, bubblewrap and network for dependency recipes)
	@[[ -n "$(T0_CHECKS)" ]] || { echo "fixtures-check: no fixtures/t0/*/check.sh found" >&2; exit 1; }
	@for c in $(T0_CHECKS); do echo "== $$c"; $$c || exit 1; done

test: go-check ## Run all tests inside the test sandbox (refuses to run unsandboxed)
	@scripts/test-sandbox.sh -- go test -count=1 ./...

test-sandbox-selftest: go-check ## Prove the test sandbox contains writes, credentials and network
	@scripts/test-sandbox-selftest.sh

ci: lint gen-check build test test-sandbox-selftest dist ## Everything CI runs

clean: ## Remove build outputs
	@rm -rf $(BIN_DIR) $(DIST_DIR)
