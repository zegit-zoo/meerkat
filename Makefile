# meerkat Makefile — convenience targets

BINARY    := meerkat
PKG       := github.com/zegit-zoo/meerkat
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Content provenance: `make sync` writes the resolved content commit to the
# stamp; KBCOMMIT reads it lazily (recursive '=') so it reflects the value
# AFTER the sync prerequisite has run. LDFLAGS is recursive for the same reason.
STAMP     := .meerkat-content-stamp
KBCOMMIT   = $(shell cat $(STAMP) 2>/dev/null || echo unknown)

LDFLAGS = -s -w \
  -X $(PKG)/internal/cli.version=$(VERSION) \
  -X $(PKG)/internal/cli.commit=$(COMMIT) \
  -X $(PKG)/internal/cli.date=$(DATE) \
  -X $(PKG)/internal/cli.kbCommit=$(KBCOMMIT)

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: kb-init
kb-init: ## Initialize the kb submodule (first checkout only)
	git submodule update --init --recursive

.PHONY: kb-update
kb-update: ## Pull the latest knowledge base content
	git submodule update --remote --recursive

.PHONY: sync
sync: ## Populate embed dirs from the source in content-source.yaml (see docs/design/content-sources.md)
	go run ./internal/contentsync

.PHONY: build
build: sync ## Build the binary for the current platform
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)
	@ln -sf $(BINARY) bin/mk
	@echo "built: bin/$(BINARY) ($(VERSION))"

.PHONY: install
install: build ## Install the binary into ~/.local/bin (atomic: write-then-rename)
	@mkdir -p $${HOME}/.local/bin
	@tmp=$$(mktemp $${HOME}/.local/bin/.$(BINARY).tmp.XXXXXX) && \
	  cp bin/$(BINARY) "$$tmp" && \
	  chmod +x "$$tmp" && \
	  mv -f "$$tmp" $${HOME}/.local/bin/$(BINARY)
	@tmplink=$$(mktemp -u $${HOME}/.local/bin/.mk.tmp.XXXXXX) && \
	  ln -s $(BINARY) "$$tmplink" && \
	  mv -f "$$tmplink" $${HOME}/.local/bin/mk
	@echo "installed: $${HOME}/.local/bin/{$(BINARY),mk}"

.PHONY: completion
completion: build ## Install zsh completion into ~/.cache/zsh/completions
	@mkdir -p $${HOME}/.cache/zsh/completions
	@./bin/$(BINARY) completion zsh > $${HOME}/.cache/zsh/completions/_$(BINARY)
	@printf '\n# share completions with the mk short alias\ncompdef _$(BINARY) mk\n' \
	    >> $${HOME}/.cache/zsh/completions/_$(BINARY)
	@printf '#compdef mk\n# delegate to the meerkat completion function\n_$(BINARY) "$$@"\n' \
	    > $${HOME}/.cache/zsh/completions/_mk
	@rm -f $${HOME}/.zcompdump*
	@echo "wrote $${HOME}/.cache/zsh/completions/{_$(BINARY),_mk}"
	@echo "run 'exec zsh' to pick up the new completions"

.PHONY: test
test: sync ## Run the test suite
	go test -race -count=1 ./...

.PHONY: test-cover
test-cover: sync ## Run tests with coverage
	go test -race -count=1 -cover -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# Ratchet floor: keep at or just below current total so it only moves up.
COVERAGE_MIN ?= 48
.PHONY: cover-check
cover-check: test-cover ## Fail if total coverage is below COVERAGE_MIN
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "total coverage: $${total}%"; \
	awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN { if (t+0 < m+0) { printf "FAIL: coverage %.1f%% is below floor %d%%\n", t, m; exit 1 } printf "OK: coverage %.1f%% >= floor %d%%\n", t, m }'

.PHONY: lint
lint: ## golangci-lint (govet, staticcheck, errcheck, gosec, gofmt/goimports, …)
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found on PATH. Install: https://golangci-lint.run/usage/install/"; \
		exit 1; \
	fi
	golangci-lint run ./...

.PHONY: pre-push
pre-push: lint test docs-check ## CI parity (lint + test + docs-check) — fast gate before git push
	@echo ""
	@echo "✓ pre-push gate green — safe to push"
	@echo "  (security stage runs only in CI; run 'make pre-release' or 'make security' to trigger locally)"

.PHONY: pre-release
pre-release: pre-push security ## Full CI parity (lint + test + docs-check + vuln + gosec + gitleaks). Slower; run before git tag.
	@echo ""
	@echo "✓ pre-release gate green — safe to tag + push"

.PHONY: install-hooks
install-hooks: ## Install a git pre-push hook that runs 'make pre-push'
	@mkdir -p .git/hooks
	@printf '#!/usr/bin/env bash\n# auto-installed by `make install-hooks`\n# skip with: git push --no-verify\nset -e\nmake pre-push\n' \
	    > .git/hooks/pre-push
	@chmod +x .git/hooks/pre-push
	@echo "installed .git/hooks/pre-push (skip with 'git push --no-verify')"

.PHONY: fmt
fmt: ## gofmt -w cmd internal
	gofmt -w cmd internal

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: smoke
smoke: build ## End-to-end smoke after a build
	@echo "--- version ---"; ./bin/$(BINARY) version
	@echo "--- list (head) ---"; ./bin/$(BINARY) list --prefix concepts/ | head -10
	@echo "--- search 'rate limiting' ---"; ./bin/$(BINARY) search "rate limiting" --limit 3
	@echo "--- show concepts/Rate-Limiting (head) ---"; ./bin/$(BINARY) show concepts/Rate-Limiting | head -8

# ---------- documentation ----------

.PHONY: docs
docs: ## Regenerate docs/CLI.md from the cobra command tree
	@echo "regenerating docs/CLI.md..."
	@go run ./internal/clidocs > docs/CLI.md
	@echo "wrote docs/CLI.md ($$(wc -l < docs/CLI.md) lines)"

.PHONY: docs-check
docs-check: ## CI gate: ensure docs/CLI.md is in sync with the cobra tree
	@go run ./internal/clidocs > /tmp/meerkat-cli.md
	@if ! diff -q /tmp/meerkat-cli.md docs/CLI.md >/dev/null; then \
		echo "ERROR: docs/CLI.md is out of sync with internal/cli/*.go"; \
		echo "       run 'make docs' to regenerate, then commit"; \
		diff -u docs/CLI.md /tmp/meerkat-cli.md | head -40; \
		exit 1; \
	fi
	@echo "docs/CLI.md is in sync"

# ---------- security scanning ----------
#
# Each target is self-installing so devs don't need a separate setup
# step. Versions are pinned to keep results reproducible across the
# team (and to keep CI fast — go install caches the binary).
#
# Severity gates: HIGH+ fails the job. MEDIUM/LOW emits a warning.
# Tune in the underlying tool config if you need to adjust.

GOVULNCHECK_VERSION := latest
GOSEC_VERSION       := v2.26.1
GITLEAKS_VERSION    := v8.21.2

GOBIN_DIR := $(shell go env GOBIN)
ifeq ($(GOBIN_DIR),)
GOBIN_DIR := $(shell go env GOPATH)/bin
endif

# tool-bin: prefer a binary already on $PATH (CI image case) but
# fall back to $GOBIN_DIR/<name> for local-dev installs.
tool-bin = $(or $(shell command -v $(1) 2>/dev/null),$(GOBIN_DIR)/$(1))

# install-tool: no-op if the tool is on $PATH. Otherwise installs
# <module-path>@<version> into $GOBIN_DIR using a stamp file so we
# don't reinstall on every run.
define install-tool
@command -v $(1) >/dev/null 2>&1 || test -f $(GOBIN_DIR)/$(1).$(3) || { \
    echo ">> installing $(1)@$(3) into $(GOBIN_DIR)"; \
    go install $(2)@$(3) && touch $(GOBIN_DIR)/$(1).$(3); \
}
endef

.PHONY: vuln
vuln: ## Scan for known CVEs in our actual import graph (govulncheck)
	$(call install-tool,govulncheck,golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))
	$(call tool-bin,govulncheck) ./...

.PHONY: gosec
gosec: sync ## Static security analysis for Go (gosec)
	$(call install-tool,gosec,github.com/securego/gosec/v2/cmd/gosec,$(GOSEC_VERSION))
	# Severity / confidence: only HIGH-severity, medium+confidence findings fail.
	# exclude-dir: skip the in-workspace go module cache (CI sets GOPATH=$$CI_PROJECT_DIR/.gopath
	#              which would otherwise drag every dep into the scan), plus dist/ and bin/,
	#              plus the two embed dirs, which hold synced markdown and yaml rather than Go.
	#              NOTE: there was also an `-exclude-dir=kb` here, left over from when the
	#              content repo was a `kb/` submodule at the repo root. gosec matches that
	#              pattern against any path component, so it was silently dropping BOTH
	#              internal/kb and internal/kbdir — the packages holding the os.Root
	#              containment and frontmatter parsing. Removed; do not reinstate.
	#
	# Excluded rules (with rationale):
	#   G304 — file inclusion via variable. Unavoidable in 'mk show' which
	#          by design loads user-supplied IDs (already scoped to embedded
	#          FS in kb.Load).
	#   G302/G306 — file permissions (0644 etc.) on the .old binary backup
	#          path. Intentional; we want the backup readable.
	$(call tool-bin,gosec) \
		-severity high \
		-confidence medium \
		-exclude=G304,G302,G306 \
		-exclude-dir=.gopath \
		-exclude-dir=.gocache \
		-exclude-dir=dist \
		-exclude-dir=bin \
		-exclude-dir=internal/kb/content \
		-exclude-dir=internal/sources/etc \
		./...

.PHONY: gitleaks
gitleaks: ## Scan git history + working tree for committed secrets (gitleaks)
	$(call install-tool,gitleaks,github.com/zricethezav/gitleaks/v8,$(GITLEAKS_VERSION))
	$(call tool-bin,gitleaks) detect --source . --redact --verbose --config .gitleaks.toml

.PHONY: security
security: vuln gosec gitleaks ## Run all security scans (vuln + gosec + gitleaks)
	@echo "✓ All security scans passed"

# ---------- release helpers ----------

.PHONY: release-check
release-check: ## Validate .goreleaser.yaml without building
	goreleaser check

.PHONY: release-snapshot
release-snapshot: sync ## Local cross-platform release build (no publish)
	KB_COMMIT=$(KBCOMMIT) goreleaser release --snapshot --clean --skip=publish

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist coverage.out coverage.xml
