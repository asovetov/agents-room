VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/asovetov/agents-room/internal/cli.Version=$(VERSION) \
           -X github.com/asovetov/agents-room/internal/mcpserver.Version=$(VERSION)

BINARIES := agents-room agents-room-mcp agents-room-hook

.PHONY: all build install test race vet fmt cross clean

all: build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/agents-room ./cmd/agents-room
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/agents-room-mcp ./cmd/agents-room-mcp
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/agents-room-hook ./cmd/agents-room-hook

# install puts the three binaries in GOBIN (or GOPATH/bin).
install:
	go install -trimpath -ldflags '$(LDFLAGS)' ./cmd/...

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
