#!/usr/bin/make -f

APP_NAME ?= perpd
BUILDDIR ?= $(CURDIR)/build
GOPATH ?= $(shell go env GOPATH)
DOCKER := $(shell which docker)
GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")

ldflags = -X github.com/cosmos/cosmos-sdk/version.Name=perpdex \
          -X github.com/cosmos/cosmos-sdk/version.AppName=$(APP_NAME) \
          -X github.com/cosmos/cosmos-sdk/version.Version=$(VERSION) \
          -X github.com/cosmos/cosmos-sdk/version.Commit=$(COMMIT)

BUILD_FLAGS := -ldflags '$(ldflags)'

.PHONY: all
all: build

###############################################################################
###                                  Build                                  ###
###############################################################################

.PHONY: build
build:
	@mkdir -p $(BUILDDIR)
	go build $(BUILD_FLAGS) -o $(BUILDDIR)/$(APP_NAME) ./cmd/perpd

.PHONY: install
install:
	go install $(BUILD_FLAGS) ./cmd/perpd

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf $(BUILDDIR)
	rm -rf $(CURDIR)/oracle-sidecar/build

###############################################################################
###                              Oracle sidecar                             ###
###############################################################################

# Build / install / run the oracle-sidecar binary that ships in
# `oracle-sidecar/`. It is a separate Go module (own go.mod) included in
# the workspace via `go.work` so `go test ./...` from the repo root sees
# both modules in CI without manual coordination.

.PHONY: build-sidecar install-sidecar run-sidecar
build-sidecar:
	$(MAKE) -C $(CURDIR)/oracle-sidecar build

install-sidecar:
	cd $(CURDIR)/oracle-sidecar && go install ./cmd/oracle-sidecar

run-sidecar:
	$(MAKE) -C $(CURDIR)/oracle-sidecar run

# `dev-stack` boots the chain and the sidecar back-to-back so a developer
# can verify the end-to-end pipeline locally. The chain runs in the
# foreground; the sidecar runs in the background and is killed on exit.
.PHONY: dev-stack
dev-stack: build build-sidecar
	@echo "[dev-stack] starting oracle-sidecar in the background"
	@$(CURDIR)/oracle-sidecar/build/oracle-sidecar --config $(CURDIR)/oracle-sidecar/oracle.json -v & \
	  SIDECAR_PID=$$!; \
	  trap "kill $$SIDECAR_PID 2>/dev/null" EXIT; \
	  echo "[dev-stack] sidecar pid $$SIDECAR_PID"; \
	  echo "[dev-stack] now boot perpd in another shell with [oracle].enabled=true"; \
	  wait $$SIDECAR_PID

###############################################################################
###                                  Lint                                   ###
###############################################################################

.PHONY: lint lint-go
lint: lint-go

lint-go:
	$(GOLANGCI_LINT) run ./...

###############################################################################
###                                  Tests                                  ###
###############################################################################

.PHONY: test test-sidecar
test:
	go test ./...

# The sidecar lives in its own Go module so `go test ./...` from the repo
# root only reaches it when a workspace is active. Use this target on CI.
test-sidecar:
	cd $(CURDIR)/oracle-sidecar && go test ./...

.PHONY: test-unit
test-unit:
	go test -count=1 ./app/... ./ante/... ./cmd/... ./types/...

.PHONY: test-integration
test-integration:
	go test -count=1 ./tests/integration/...

.PHONY: test-race
test-race:
	go test -race -count=1 ./...

###############################################################################
###                                  Proto                                  ###
###############################################################################

protoVer       := 0.15.2
protoImageName := ghcr.io/cosmos/proto-builder:$(protoVer)
protoImage     := $(DOCKER) run --rm -v $(CURDIR):/workspace --workdir /workspace $(protoImageName)

.PHONY: proto-all proto-gen proto-swagger-gen proto-format proto-lint proto-check-breaking proto-update-deps

proto-all: proto-format proto-lint proto-gen

proto-gen:
	@echo "Generating Protobuf files"
	@$(protoImage) sh ./proto/scripts/protocgen.sh

proto-swagger-gen:
	@echo "Generating Protobuf Swagger"
	@$(protoImage) sh ./proto/scripts/protoc-swagger-gen.sh

proto-format:
	@$(protoImage) find ./ -name "*.proto" -exec clang-format -i {} \;

proto-lint:
	@$(protoImage) buf lint --error-format=json

proto-check-breaking:
	@$(protoImage) buf breaking --against $(HTTPS_GIT)#branch=main

proto-update-deps:
	@echo "Updating Protobuf dependencies"
	$(DOCKER) run --rm -v $(CURDIR)/proto:/workspace --workdir /workspace $(protoImageName) buf mod update
