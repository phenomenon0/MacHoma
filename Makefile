GO ?= go
CC ?= cc
BUILD_DIR ?= build
SUDO ?= sudo -n

# Do not inherit a surrounding workspace's dependency overrides.
export GOWORK = off

.PHONY: all build test race vet check cross linux-peer raw-selftest help

all: build

build:
	mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -o $(BUILD_DIR)/homa-echo ./cmd/homa-echo

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: test race vet

cross:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -o $(BUILD_DIR)/homa-echo-darwin-arm64 ./cmd/homa-echo
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -o $(BUILD_DIR)/homa-echo-darwin-amd64 ./cmd/homa-echo
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -o $(BUILD_DIR)/homa-echo-linux-amd64 ./cmd/homa-echo
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -o $(BUILD_DIR)/homa-echo-linux-arm64 ./cmd/homa-echo

# Linux-only C peer for a separately installed Homa kernel module. Its self-test
# checks buffer reconstruction without opening sockets or changing the kernel.
linux-peer:
	mkdir -p $(BUILD_DIR)
	$(CC) -std=c11 -O2 -Wall -Wextra -Werror -pedantic -o $(BUILD_DIR)/homa-linux-peer interop/linux_peer.c
	$(BUILD_DIR)/homa-linux-peer --self-test

# Explicit opt-in to raw-socket privileges. Uses only 127.0.0.1:4000/4001.
# This proves userspace raw-IP loopback, not Linux kernel interoperability.
raw-selftest: build
	$(SUDO) "$(BUILD_DIR)/homa-echo" selftest

help:
	@printf '%s\n' 'make build         Build the local native Homa echo CLI' 'make check         Run tests, race detector, and go vet' 'make cross         Build macOS/Linux arm64/amd64 CLIs' 'make linux-peer    Build and self-test the Linux kernel peer helper' 'make raw-selftest  Run privileged userspace raw-IP loopback only'
