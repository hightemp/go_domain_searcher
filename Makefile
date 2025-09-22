# Makefile for domain_search CLI

.DEFAULT_GOAL := help
SHELL := /bin/bash

APP ?= domain_search
BIN ?= $(APP)
CONFIG ?= ./domain_search.config.yaml
OUT ?= domains
LOOP ?= false
GO ?= go
LDFLAGS ?= -s -w

.PHONY: all help tidy build run build-linux build-macos build-windows clean clean-results vet test

all: build

help:
	@echo "Targets:"
	@echo "  make build            - Build $(BIN)"
	@echo "  make run              - Run with flags (-config, -out, -loop)"
	@echo "  make tidy             - Run 'go mod tidy'"
	@echo "  make vet              - Run 'go vet ./...'"
	@echo "  make test             - Run 'go test ./...'"
	@echo "  make build-linux      - Build Linux amd64 binary"
	@echo "  make build-macos      - Build macOS arm64 binary"
	@echo "  make build-windows    - Build Windows amd64 binary"
	@echo "  make clean            - Remove build artifacts"
	@echo "  make clean-results    - Remove $(OUT) directory"
	@echo ""
	@echo "Variables:"
	@echo "  BIN=$(BIN) CONFIG=$(CONFIG) OUT=$(OUT) LOOP=$(LOOP)"
	@echo "Examples:"
	@echo "  make run"
	@echo "  make run CONFIG=./domain_search.config.yaml OUT=domains LOOP=true"
	@echo "  make build-linux BIN=domain_search"

tidy:
	$(GO) mod tidy

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

build: tidy
	$(GO) build -v -o $(BIN) .

run:
	$(GO) run . -config $(CONFIG) -out $(OUT) $(if $(filter true,$(LOOP)),-loop,)

build-linux:
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BIN)-linux-amd64 .

build-macos:
	GOOS=darwin GOARCH=arm64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BIN)-darwin-arm64 .

build-windows:
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags="$(LDFLAGS)" -o $(BIN)-windows-amd64.exe .

clean:
	rm -f $(BIN) $(BIN)-linux-amd64 $(BIN)-darwin-arm64 $(BIN)-windows-amd64.exe

clean-results:
	rm -rf $(OUT)