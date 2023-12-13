TEST?=$$(go list ./... |grep -v 'vendor')
GOFMT_FILES?=$$(find . -name '*.go' |grep -v vendor)
GO_CMD ?= go
APP_NAME = grantm
BUILD_DIR = $(PWD)/build
SHELL := /bin/bash

clean:
	rm -rf ./build ./dist

tidy:
	go mod tidy

fmt:
	$(GO_CMD)fmt -w $(GOFMT_FILES)

lint:
	golangci-lint run

security:
	gosec -exclude-dir _local -quiet ./...

build:
	promu build
