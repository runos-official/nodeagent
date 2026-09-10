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

.PHONY: format-check
format-check:
	@gofmt_output="$$(gofmt -l .)" && { [ -z "$$gofmt_output" ] || { printf '%s\n' "$$gofmt_output"; false; }; }

.PHONY: version
version:
	@echo "$(VERSION)"

.PHONY: clean
clean:
	@rm -f $(BINARY_NAME)

# ============================================================================
# Leak gate (PUBLIC repo)
# ============================================================================

# Install the tracked git hooks for this clone. .git/hooks is not tracked, so
# every clone must do this once. Run it right after you clone.
.PHONY: hooks
hooks:
	@git config core.hooksPath .githooks
	@echo "$(GREEN)core.hooksPath = .githooks$(NC) (pre-commit now runs leakcheck on the staged diff)"

# Scan every tracked file for credentials and un-baselined internal identifiers.
.PHONY: leakcheck
leakcheck:
	@python3 scripts/leakcheck.py

# Scan only the staged diff, the same way the pre-commit hook does.
.PHONY: leakcheck-staged
leakcheck-staged:
	@python3 scripts/leakcheck.py --staged

# Ratchet the baseline down after you REMOVE an identifier from the source.
# Never run this to get a new identifier past the gate.
.PHONY: leakcheck-update
leakcheck-update:
	@python3 scripts/leakcheck.py --update

# Test the checker itself: what it must catch and what it must not.
.PHONY: leakcheck-test
leakcheck-test:
	@python3 scripts/leakcheck_test.py

# Fail on a tracked file leakcheck cannot READ. leakcheck reads UTF-8 text; a
# file holding a NUL byte or other bytes is read best effort at most, and some
# encodings escape it entirely (measured on checker 1.2.0: a token in a UTF-32
# file, and a token broken up by NUL bytes, both pass with exit 0). This target
# turns such a file into a decision somebody records, not a silent gap.
.PHONY: unscannable
unscannable:
	@python3 scripts/unscannable_check.py

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
	@echo "  make format-check Reject unformatted Go files"
	@echo "  make version  Show the version that would be injected"
	@echo "  make clean    Remove build artifacts"
	@echo ""
	@echo "  make hooks            Install the tracked git hooks (run once per clone)"
	@echo "  make leakcheck        Scan every tracked file for leaks (PUBLIC repo gate)"
	@echo "  make leakcheck-staged Scan only the staged diff"
	@echo "  make leakcheck-update Ratchet the baseline down after removing an identifier"
	@echo "  make leakcheck-test   Test the leak checker itself"
	@echo "  make unscannable      Fail on a tracked file leakcheck cannot read"
	@echo ""
	@echo "  make release RELEASE_VERSION=vX.Y.Z         Cut a release (gates, tag, push, verify)"
	@echo "  make release RELEASE_VERSION=vX.Y.Z CHECK=1 Run release gates only, no tag/push"
	@echo ""
	@echo "Release runs scripts/release.sh: gates, tag the dev commit, push, watch the"
	@echo "Release workflow, and verify the build-provenance attestation."
