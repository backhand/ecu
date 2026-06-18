# ECU — Easy Computer Use. Build / test / package targets.
#
# Quick start:
#   make            # show this help
#   make build      # build the control-plane binary -> bin/ecu
#   make check      # fmt-check + vet + test (the CI gate)
#   make cross      # cross-compile release binaries -> dist/ecu-<os>-<arch>
#   make images     # build both container images
#
# The cross/image targets mirror what the release pipeline publishes
# (ecu-<os>-<arch> assets + the container images); see README.md / install.sh.

# ---- configuration --------------------------------------------------------
BINARY      := ecu
PKG         := ./cmd/ecu
BIN_DIR     := bin
DIST_DIR    := dist

# Stamped into image tags (and ready for ldflags -X if a version symbol is
# added later). Derived from git; falls back to "dev".
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Pure Go (modernc sqlite) — keep CGO off so the binary is static and
# cross-compiles cleanly, matching the single-static-binary promise.
export CGO_ENABLED := 0
GO          ?= go
GOFLAGS     ?=
LDFLAGS     ?= -s -w

# Container images.
IMAGE_TAG          ?= dev
INSTANCE_IMAGE     ?= ecu-image:$(IMAGE_TAG)
CONTROLPLANE_IMAGE ?= ecu-controlplane:$(IMAGE_TAG)
# The instance image builds FROM an amd64-only base, so pin the platform.
INSTANCE_PLATFORM  ?= linux/amd64

# Cross-compile matrix (matches install.sh's ecu-<os>-<arch> asset names).
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

SKILL_DIR := skill/ecu-computer-use

.DEFAULT_GOAL := help

# ---- build / test ---------------------------------------------------------
.PHONY: build
build: ## Build the control-plane binary -> bin/ecu
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(PKG)

.PHONY: test
test: ## Run all Go tests
	$(GO) test ./...

.PHONY: race
race: ## Run all Go tests under the race detector
	$(GO) test -race ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format Go sources in place (gofmt -w)
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: check
check: fmt-check vet test ## fmt-check + vet + test (the CI gate)

# ---- cross-compile / release ---------------------------------------------
.PHONY: cross
cross: ## Cross-compile release binaries -> dist/ecu-<os>-<arch>
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=$(DIST_DIR)/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done

# ---- container images -----------------------------------------------------
.PHONY: image
image: ## Build the instance (computer-use) container image
	docker build --platform $(INSTANCE_PLATFORM) -t $(INSTANCE_IMAGE) image/

.PHONY: controlplane-image
controlplane-image: ## Build the control-plane container image
	docker build -t $(CONTROLPLANE_IMAGE) -f Dockerfile .

.PHONY: images
images: image controlplane-image ## Build both container images

# ---- skill (Python client) ------------------------------------------------
.PHONY: skill-test
skill-test: ## Run the Python skill unit tests (needs: pip install "mcp[cli]" requests Pillow)
	cd $(SKILL_DIR) && python3 -m unittest -v test_ecu_client

# ---- housekeeping ---------------------------------------------------------
.PHONY: clean
clean: ## Remove build artifacts (bin/, dist/)
	rm -rf $(BIN_DIR) $(DIST_DIR)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'
