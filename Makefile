BINARY_NAME=gosync
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)"

GOOS_LIST=linux darwin windows
GOARCH_LIST=amd64 arm64

SHELL=/bin/bash

.PHONY: all clean test lint build

all: build

build:
	go build $(LDFLAGS) -o $(BINARY_NAME) ./cmd/gosync

test:
	go test -v -race -count=1 ./...

test-cover:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY_NAME)
	rm -rf dist/
	rm -f coverage.out coverage.html

dist: clean
	@mkdir -p dist
	@for os in $(GOOS_LIST); do \
		for arch in $(GOARCH_LIST); do \
			echo "Building $$os/$$arch..."; \
			GOOS=$$os GOARCH=$$arch go build $(LDFLAGS) -o dist/$(BINARY_NAME)-$$os-$$arch ./cmd/gosync; \
		done; \
	done
	@echo "Builds complete in dist/"

dist-linux: clean
	@mkdir -p dist
	@for arch in $(GOARCH_LIST); do \
		echo "Building linux/$$arch..."; \
		GOOS=linux GOARCH=$$arch go build $(LDFLAGS) -o dist/$(BINARY_NAME)-linux-$$arch ./cmd/gosync; \
	done

dist-darwin: clean
	@mkdir -p dist
	@for arch in $(GOARCH_LIST); do \
		echo "Building darwin/$$arch..."; \
		GOOS=darwin GOARCH=$$arch go build $(LDFLAGS) -o dist/$(BINARY_NAME)-darwin-$$arch ./cmd/gosync; \
	done

dist-windows: clean
	@mkdir -p dist
	@for arch in $(GOARCH_LIST); do \
		echo "Building windows/$$arch..."; \
		GOOS=windows GOARCH=$$arch go build $(LDFLAGS) -o dist/$(BINARY_NAME)-windows-$$arch.exe ./cmd/gosync; \
	done

install:
	go install $(LDFLAGS) ./cmd/gosync

version:
	@echo "$(VERSION) ($(COMMIT)) built $(BUILD_DATE)"
