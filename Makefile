TEST?=$$(go list ./... |grep -v 'vendor')
GO           ?= go
GOFMT        ?= $(GO)fmt
GOFMT_FILES?=$$(find . -name '*.go' |grep -v vendor)
SHELL := /bin/bash
FIRST_GOPATH := $(firstword $(subst :, ,$(shell $(GO) env GOPATH)))
PROMU        := $(FIRST_GOPATH)/bin/promu

include Makefile.common

clean:
	rm -rf ./build ./dist

tidy:
	go mod tidy

fmt:
	$(GOFMT) -w $(GOFMT_FILES)

lint:
	golangci-lint run

security:
	gosec -exclude-dir _local -quiet ./...

# Regenerate cache.pb.go from cache.proto. Requires protoc and protoc-gen-gogofast
# (go install github.com/gogo/protobuf/protoc-gen-gogofast@latest) on PATH.
proto:
	protoc --gogofast_out=paths=source_relative:. cache.proto

promu:
	$(PROMU) build
