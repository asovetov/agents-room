VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/asovetov/agents-room/internal/cli.Version=$(VERSION) \
           -X github.com/asovetov/agents-room/internal/mcpserver.Version=$(VERSION)

BINARIES := agents-room

.PHONY: all build install test race vet fmt cross clean crosscheck

all: build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/agents-room ./cmd/agents-room

# install puts the single binary in GOBIN (or GOPATH/bin).
install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/agents-room

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Cross-compile every supported target into dist/.
cross:
	@rm -rf dist
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		for b in $(BINARIES); do \
			echo "  $$os/$$arch $$b"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
				-ldflags '$(LDFLAGS)' \
				-o dist/$$b-$$os-$$arch$$ext ./cmd/$$b || exit 1; \
		done; \
	done

clean:
	rm -rf bin dist

# crosscheck compiles every package and every test binary for the other
# supported platforms.
#
# It cannot run them: Windows needs wine and macOS needs the OS. What it does
# catch is the class of breakage that is invisible on Linux and only shows up
# in CI — a file that is missing its build tag, a test that calls a function
# that exists only on one platform, a syscall wrapped behind the wrong
# //go:build line. Two bugs in 0.5.x failed only on the Windows runner, and
# both would have been caught here in seconds.
crosscheck:
	@for target in darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  $$os/$$arch build"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build ./... || exit 1; \
		echo "  $$os/$$arch vet"; \
		GOOS=$$os GOARCH=$$arch go vet ./... || exit 1; \
		echo "  $$os/$$arch test binaries"; \
		for p in $$(GOOS=$$os GOARCH=$$arch go list ./...); do \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go test -c -o /dev/null $$p || exit 1; \
		done; \
	done
	@echo "crosscheck: every target compiles"
