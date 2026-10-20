# meerkat Makefile — convenience targets

# ---------- toolchain ----------
#
# go.mod's `toolchain` line is the single source of truth for which Go
# builds this project. `export GOTOOLCHAIN` makes every `go` command a
# recipe below runs — including the ones inside golangci-lint and the
# scanners — use exactly that toolchain, with no per-developer setup.
#
# Why it has to be set at all: a `toolchain` directive only ratchets
# *up*. A host Go NEWER than the pin already satisfies go.mod, so it is
# used as-is. Homebrew ships Go 1.27.x today, so an unpinned `make lint`
# typechecked this code against the 1.27 standard library and died on
# `math/rand/v2` signatures the pinned 1.26.9 does not have. Homebrew
# also installs its own GOROOT/go.env with a GOTOOLCHAIN default in it,
# so `go env GOTOOLCHAIN` is not necessarily what upstream Go ships.
#
# CI is unaffected and downloads nothing: actions/setup-go already runs
# with `go-version-file: go.mod`, so the toolchain named here is the one
# the runner is executing.
#
# `?=` so an explicit `GOTOOLCHAIN=go1.27.1 make lint` from the
# environment still wins — make does not let `?=` override a variable it
# inherited from the environment. Useful when bisecting a toolchain
# regression; the default stays the pin.
GOTOOLCHAIN ?= $(shell awk '/^toolchain /{print $$2; found=1; exit} END{if (!found) print "auto"}' go.mod)
export GOTOOLCHAIN

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

# ---------- pinned tooling ----------
#
# Every tool a gate runs is installed by this Makefile into its own
# version-named directory under .tools/ and invoked from there by
# absolute path; $PATH is never consulted. The directory IS the stamp:
# if .tools/<name>@<version>/<name> exists, GOBIN pointed there for
# exactly that `go install <module>@<version>`, so it cannot be anything
# else. .tools/ is gitignored and excluded from the gosec walk, and
# `make clean` deliberately leaves it alone — the tool builds are too
# expensive to redo on every clean; `make clean-tools` drops them when
# you want that. The scanner pins live in the security section below.

TOOLS_DIR := $(CURDIR)/.tools

# tool-install: install <module-path>@<version> into
# .tools/<name>@<version>/ unless that binary is already there. GOBIN is
# set per install, so the directory name and the binary inside it cannot
# disagree. Re-runs are free; the module cache makes a cold install fast.
define tool-install
@test -x $(TOOLS_DIR)/$(1)@$(3)/$(1) || { \
    echo ">> installing $(1)@$(3) into $(TOOLS_DIR)/$(1)@$(3)"; \
    GOBIN=$(TOOLS_DIR)/$(1)@$(3) go install $(2)@$(3); \
}
endef

# golangci-lint is pinned HERE and nowhere else. It used to be named in
# three places that could drift apart: ci.yml and release.yml each
# `go install`ed v2.14.0 onto $PATH before calling `make lint`, the
# pre-commit hook carried its own `rev:`, and `make lint` itself ran
# whatever `golangci-lint` was on $PATH — which is not a pin at all, just
# a dependency on whatever the developer's package manager last shipped.
# Now all three routes run this one binary: the workflows call `make
# lint`, and the pre-commit hook is a `language: system` hook whose entry
# is `make lint`.
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint@$(GOLANGCI_LINT_VERSION)/golangci-lint

# The 5m budget the old pre-commit hook passed as `--timeout 5m` is
# already `run.timeout: 5m` in .golangci.yml, so it carries over here
# rather than being duplicated on the command line.
.PHONY: lint
lint: ## golangci-lint, pinned + self-installing (govet, staticcheck, errcheck, gosec, gofmt/goimports, …)
	$(call tool-install,golangci-lint,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	$(GOLANGCI_LINT) run ./...

.PHONY: lint-config
lint-config: ## Validate .golangci.yml against the pinned golangci-lint's schema
	$(call tool-install,golangci-lint,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	$(GOLANGCI_LINT) config verify

# toolchain-check keeps the Dockerfile's Go pin honest. go.mod's
# `toolchain` line is the single source of truth for every Go gate, but
# the release image builds from its own `ARG GO_VERSION` (plus a
# golang base-image digest) that nothing else reads, and Dependabot has
# no docker ecosystem here, so a toolchain bump would silently leave
# the image on the old Go (#80 review). Bump both, and the digest, in
# one change.
.PHONY: toolchain-check
toolchain-check: ## Fail if the Dockerfile's GO_VERSION disagrees with go.mod's toolchain
	@want=$$(awk '/^toolchain /{sub(/^go/,"",$$2); print $$2}' go.mod); \
	got=$$(awk -F= '/^ARG GO_VERSION=/{print $$2}' Dockerfile); \
	if [ -z "$$want" ] || [ "$$want" != "$$got" ]; then \
		echo "FAIL: Dockerfile ARG GO_VERSION=$$got but go.mod toolchain is go$$want — bump both, and the golang base-image digest"; exit 1; \
	fi; \
	echo "OK: Dockerfile GO_VERSION matches go.mod toolchain ($$want)"

# hygiene re-runs the commit-stage hygiene hooks over the whole tree.
# They have no CI job of their own (golangci-lint, gitleaks and
# markdownlint do), so a commit made without hooks — --no-verify, the
# web UI, a clone that never ran `pre-commit install` — would skip them
# entirely (#80 review). Hooks are named, not SKIPped, so this target can
# never quietly drop a security hook. PRE_COMMIT lets CI run a pinned
# pre-commit through pipx.
HYGIENE_HOOKS := end-of-file-fixer trailing-whitespace check-yaml check-merge-conflict check-added-large-files
PRE_COMMIT ?= pre-commit
.PHONY: hygiene
hygiene: ## Run the commit-stage hygiene hooks over the whole tree (CI backstop)
	@for h in $(HYGIENE_HOOKS); do $(PRE_COMMIT) run $$h --all-files --show-diff-on-failure || exit 1; done

# chart-check validates charts/meerkat offline-ish: `helm lint --strict`
# and `helm template | kubeconform -strict` for the defaults and every
# values file under charts/meerkat/ci/, then one values.schema.json
# negative case that must FAIL to render. kubeconform fetches schemas:
# core kinds from its default location, ServiceMonitor and HTTPRoute
# from the datree CRDs catalog pinned to one commit. Without helm or
# kubeconform on PATH it skips, unless CHART_TOOLS_REQUIRED=1 (CI).
# The render goes to kubeconform on stdin: given a file path, v0.8.0
# skips any name without a .yaml/.yml/.json suffix and reports
# "0 resource found" with exit 0. CustomResourceDefinition is skipped:
# neither location publishes its schema, and the operator CRDs the chart
# renders are controller-gen output the envtest job applies to a real
# API server.
CHART            := charts/meerkat
HELM             ?= helm
KUBECONFORM      ?= kubeconform
CHART_KUBE_VERSION := 1.31.0
CRDS_CATALOG_REF := 63669a570e231d4f1f8396d229a1de512bcf0a34
CRDS_CATALOG     := https://raw.githubusercontent.com/datreeio/CRDs-catalog/$(CRDS_CATALOG_REF)/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json
.PHONY: chart-check
chart-check: ## helm lint + template | kubeconform over charts/meerkat and its ci/ values
	@for t in $(HELM) $(KUBECONFORM); do \
		if ! command -v $$t >/dev/null 2>&1; then \
			if [ -n "$(CHART_TOOLS_REQUIRED)" ]; then echo "FAIL: $$t not on PATH"; exit 1; fi; \
			echo "SKIP: chart-check needs $$t on PATH"; exit 0; \
		fi; \
	done; \
	set -e; \
	out=$$(mktemp); trap 'rm -f "$$out"' EXIT; \
	for v in "" $(CHART)/ci/*-values.yaml; do \
		f=$${v:+-f $$v}; \
		echo "--- $${v:-defaults}"; \
		$(HELM) lint --strict $(CHART) $$f; \
		$(HELM) template chart-check $(CHART) $$f > "$$out"; \
		grep -q '^kind: Deployment$$' "$$out" || { echo "FAIL: no Deployment rendered"; exit 1; }; \
		$(KUBECONFORM) -strict -summary -kubernetes-version $(CHART_KUBE_VERSION) -skip CustomResourceDefinition \
			-schema-location default -schema-location '$(CRDS_CATALOG)' < "$$out"; \
	done; \
	if $(HELM) template chart-check $(CHART) --set server.mode=grpc >/dev/null 2>&1; then \
		echo "FAIL: values.schema.json accepted server.mode=grpc"; exit 1; \
	fi; \
	echo "OK: schema rejects server.mode=grpc"

.PHONY: pre-push
pre-push: lint test docs-check ## CI parity (lint + test + docs-check) — fast gate before git push
	@echo ""
	@echo "✓ pre-push gate green — safe to push"
	@echo "  (security stage runs only in CI; run 'make pre-release' or 'make security' to trigger locally)"

# pre-release is every CI gate that runs without external services. It
# runs the suite through cover-check rather than test, so the coverage
# floor — a CI-only gate otherwise — is covered locally too (#81 review),
# without running the race suite twice.
.PHONY: pre-release
pre-release: lint cover-check docs-check lint-config toolchain-check hygiene security ## Full CI parity (lint + coverage floor + docs-check + lint-config + toolchain + hygiene + vuln + gosec + gitleaks). Slower; run before git tag.
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

# ---------- operator CRDs (meerkat-mob#10) ----------
#
# The KnowledgeBase and MeerkatMob CRDs and their deep-copy methods are
# generated from internal/operator/api by controller-gen, pinned here
# like golangci-lint. `make crds` regenerates them; `make crds-check`
# fails when a types change was committed without regenerating.
CONTROLLER_GEN_VERSION := v0.23.0
CONTROLLER_GEN := $(TOOLS_DIR)/controller-gen@$(CONTROLLER_GEN_VERSION)/controller-gen
CRD_DIR := deploy/operator/crds
OPERATOR_RBAC_DIR := deploy/operator/rbac
OPERATOR_WEBHOOK_DIR := deploy/operator/webhook
# charts/meerkat renders the operator from copies of the generated files
# (Helm reads files only from inside the chart), refreshed by `make crds`.
CHART_OPERATOR_DIR := charts/meerkat/files/operator

.PHONY: crds
crds: ## Regenerate the operator CRDs and deep-copy methods from internal/operator/api
	$(call tool-install,controller-gen,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))
	$(CONTROLLER_GEN) object paths=./internal/operator/api/...
	$(CONTROLLER_GEN) crd paths=./internal/operator/api/... output:crd:artifacts:config=$(CRD_DIR)
	$(CONTROLLER_GEN) rbac:roleName=meerkat-operator webhook paths="./internal/operator/...;./cmd/meerkat-operator/..." \
		output:rbac:artifacts:config=$(OPERATOR_RBAC_DIR) output:webhook:artifacts:config=$(OPERATOR_WEBHOOK_DIR)
	rm -rf $(CHART_OPERATOR_DIR)
	mkdir -p $(CHART_OPERATOR_DIR)/crds
	cp $(CRD_DIR)/*.yaml $(CHART_OPERATOR_DIR)/crds/
	cp $(OPERATOR_RBAC_DIR)/role.yaml $(OPERATOR_WEBHOOK_DIR)/manifests.yaml $(CHART_OPERATOR_DIR)/

.PHONY: crds-check
crds-check: crds ## CI gate: fail if the generated CRDs or deep-copy methods are stale
	@if [ -n "$$(git status --porcelain -- $(CRD_DIR) $(OPERATOR_RBAC_DIR) $(OPERATOR_WEBHOOK_DIR) $(CHART_OPERATOR_DIR) internal/operator/api)" ]; then \
		echo "ERROR: generated operator files are out of date; run 'make crds' and commit"; \
		git status --short -- $(CRD_DIR) $(OPERATOR_RBAC_DIR) $(OPERATOR_WEBHOOK_DIR) $(CHART_OPERATOR_DIR) internal/operator/api; \
		exit 1; \
	fi
	@echo "operator CRDs are in sync"

# The operator's envtest suite runs the reconciler and the webhook
# against a real kube-apiserver and etcd. setup-envtest has no tagged
# releases, so it is pinned to a commit of controller-runtime's
# release-0.25 branch (the controller-runtime minor in go.mod), and the
# binaries to the Kubernetes minor of k8s.io/client-go. setup-envtest
# checks each download against the release index's digest. Without
# KUBEBUILDER_ASSETS, `make test` skips TestEnvtest.
SETUP_ENVTEST_VERSION := v0.25.3-0.20261012174806-32759fab3119
SETUP_ENVTEST := $(TOOLS_DIR)/setup-envtest@$(SETUP_ENVTEST_VERSION)/setup-envtest
ENVTEST_K8S_VERSION := 1.37.0

.PHONY: test-envtest
test-envtest: sync ## Run the operator tests against a real API server (envtest)
	$(call tool-install,setup-envtest,sigs.k8s.io/controller-runtime/tools/setup-envtest,$(SETUP_ENVTEST_VERSION))
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOLS_DIR)/envtest -p path)" \
		go test -race -count=1 ./internal/operator/... ./cmd/meerkat-operator/...

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
# Each scanner self-installs through the shared .tools mechanism in
# "pinned tooling" above, so devs don't need a separate setup step, and
# the version below is what actually runs — locally and in CI.
#
# $PATH is never consulted. These targets used to prefer whatever
# gosec/gitleaks/govulncheck was already on $PATH, so a Homebrew install
# won silently and the pinned version below was a comment rather than a
# fact; the $GOBIN/<tool>.<version> stamp files made it worse by claiming
# a version for a binary they sat next to but did not describe (a stamp
# for one gosec was found beside a gosec of another vintage).
#
# Severity gates: HIGH+ fails the job. MEDIUM/LOW emits a warning.
# Tune in the underlying tool config if you need to adjust.

# govulncheck is pinned like the rest. The vulnerability DATABASE is
# still fetched live from vuln.go.dev on every run, so pinning the
# checker costs no freshness — it only stops the scanner itself from
# changing underneath a release.
GOVULNCHECK_VERSION := v1.8.0
GOSEC_VERSION       := v2.29.0
GITLEAKS_VERSION    := v8.30.1

GOVULNCHECK := $(TOOLS_DIR)/govulncheck@$(GOVULNCHECK_VERSION)/govulncheck
GOSEC       := $(TOOLS_DIR)/gosec@$(GOSEC_VERSION)/gosec
GITLEAKS    := $(TOOLS_DIR)/gitleaks@$(GITLEAKS_VERSION)/gitleaks

.PHONY: vuln
vuln: ## Scan for known CVEs in our actual import graph (govulncheck)
	$(call tool-install,govulncheck,golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))
	$(GOVULNCHECK) ./...

.PHONY: gosec
gosec: sync ## Static security analysis for Go (gosec)
	$(call tool-install,gosec,github.com/securego/gosec/v2/cmd/gosec,$(GOSEC_VERSION))
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
	$(GOSEC) \
		-severity high \
		-confidence medium \
		-exclude=G304,G302,G306 \
		-exclude-dir=.tools \
		-exclude-dir=.gopath \
		-exclude-dir=.gocache \
		-exclude-dir=dist \
		-exclude-dir=bin \
		-exclude-dir=internal/kb/content \
		-exclude-dir=internal/sources/etc \
		./...

.PHONY: gitleaks
gitleaks: ## Scan git history + working tree for committed secrets (gitleaks)
	$(call tool-install,gitleaks,github.com/zricethezav/gitleaks/v8,$(GITLEAKS_VERSION))
	$(GITLEAKS) detect --source . --redact --verbose --config .gitleaks.toml

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
clean: ## Remove build artifacts (leaves .tools — see clean-tools)
	rm -rf bin dist coverage.out coverage.xml

.PHONY: clean-tools
clean-tools: ## Remove the repo-local scanner installs in .tools
	rm -rf $(TOOLS_DIR)
