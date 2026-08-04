APP_NAME := cronmate
CMD_PATH := ./cmd/$(APP_NAME)
BIN_DIR ?= bin
BINARY := $(BIN_DIR)/$(APP_NAME)
GO ?= go

.DEFAULT_GOAL := build

.PHONY: build run install test fmt vet check tidy clean

build:
	@mkdir -p "$(BIN_DIR)"
	$(GO) build -o "$(BINARY)" "$(CMD_PATH)"

run:
	$(GO) run "$(CMD_PATH)" $(ARGS)

install:
	$(GO) install "$(CMD_PATH)"

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

check: test vet

tidy:
	$(GO) mod tidy

clean:
	rm -rf "$(BIN_DIR)"
