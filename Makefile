#!/usr/bin/make -f

APP_NAME ?= perpd
BUILDDIR ?= $(CURDIR)/build
GOPATH ?= $(shell go env GOPATH)
DOCKER := $(shell which docker)

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

###############################################################################
###                                  Tests                                  ###
###############################################################################

.PHONY: test
test:
	go test ./...

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
