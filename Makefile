GO          ?= go
BIN         := amber_exporter
VERSION     ?= $(shell cat VERSION)
REVISION    ?= $(shell git rev-parse --verify -q HEAD || echo unknown)
BRANCH      ?= $(shell git symbolic-ref -q --short HEAD || echo unknown)
BUILD_USER  ?= $(shell whoami)@$(shell hostname)
BUILD_DATE  ?= $(shell date -u +%Y%m%d-%H:%M:%S)
DOCKER_IMAGE ?= amber_exporter

VERSION_PKG := github.com/prometheus/common/version
LDFLAGS     := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Revision=$(REVISION) \
	-X $(VERSION_PKG).Branch=$(BRANCH) \
	-X $(VERSION_PKG).BuildUser=$(BUILD_USER) \
	-X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

.PHONY: all
all: format-check vet test build

.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

.PHONY: test
test:
	$(GO) test -race ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: format
format:
	gofmt -w .

.PHONY: format-check
format-check:
	@test -z "$$(gofmt -l .)" || (echo "These files need gofmt:"; gofmt -l .; exit 1)

.PHONY: docker
docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(REVISION) \
		--build-arg BRANCH=$(BRANCH) \
		-t $(DOCKER_IMAGE):$(VERSION) .
