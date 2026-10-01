# cephfs-fe is pure Go (no cgo): a static binary that runs on any
# x86_64 Linux host that can reach the cephfs-index database.

GO ?= go

.PHONY: all build test vet clean

all: vet test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o bin/cephfs-fe .

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf bin
