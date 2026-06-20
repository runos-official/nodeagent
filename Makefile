# Makefile for the RunOS node agent

# Color codes for output
GREEN := \033[0;32m
RED := \033[0;31m
BLUE := \033[0;34m
CYAN := \033[0;36m
GRAY := \033[0;90m
NC := \033[0m

# Version is the latest git tag with its leading "v" stripped (falls back to
# "dev"). It is injected into the binary at build time via -ldflags, matching
# what the release pipeline does.
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo "dev")
LDFLAGS := -X github.com/runos-official/nodeagent/version.Version=$(VERSION)

BINARY_NAME := nodeagent

# Default target
.DEFAULT_GOAL := help

# ============================================================================
# Development
# ============================================================================

.PHONY: build
build:
	@go build -ldflags="$(LDFLAGS)" -o $(BINARY_NAME) .

.PHONY: test
test:
	@echo "$(GRAY)[`date '+%H:%M:%S'`]$(NC) $(BLUE)Running tests...$(NC)"
	@go test -race ./...
	@echo "$(GRAY)[`date '+%H:%M:%S'`]$(NC) $(GREEN)Tests passed$(NC)"

.PHONY: vet
vet:
	@go vet ./...

.PHONY: version
version:
	@echo "$(VERSION)"

.PHONY: clean
clean:
	@rm -f $(BINARY_NAME)

# ============================================================================
# Release
# ============================================================================

# Cut a release: runs every gate, tags the dev commit, pushes, watches the
# Release workflow, and verifies the published binary's attestation.
# Usage: make release RELEASE_VERSION=v0.24.0   (add CHECK=1 to run gates only)
.PHONY: release
release:
	@test -n "$(RELEASE_VERSION)" || { echo "$(RED)set RELEASE_VERSION, e.g. make release RELEASE_VERSION=v0.24.0$(NC)"; exit 1; }
	@scripts/release.sh $(RELEASE_VERSION) $(if $(CHECK),--check,)

# ============================================================================
# Help
# ============================================================================

.PHONY: help
help:
	@echo "$(CYAN)RunOS Node Agent$(NC)"
	@echo ""
	@echo "  make build    Build the binary for the current platform (version injected)"
	@echo "  make test     Run the test suite (go test -race ./...)"
	@echo "  make vet      Run go vet"
	@echo "  make version  Show the version that would be injected"
	@echo "  make clean    Remove build artifacts"
	@echo ""
	@echo "  make release RELEASE_VERSION=vX.Y.Z         Cut a release (gates, tag, push, verify)"
	@echo "  make release RELEASE_VERSION=vX.Y.Z CHECK=1 Run release gates only, no tag/push"
	@echo ""
	@echo "Release runs scripts/release.sh: gates, tag the dev commit, push, watch the"
	@echo "Release workflow, and verify the build-provenance attestation."
