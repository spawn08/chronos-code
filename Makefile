MODULE   := github.com/spawn08/chronos-code
BINARY   := chronos-code
BIN_DIR  := bin

VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS  := -s -w \
	-X '$(MODULE)/internal/cli.Version=$(VERSION)' \
	-X '$(MODULE)/internal/cli.Commit=$(COMMIT)' \
	-X '$(MODULE)/internal/cli.BuildDate=$(BUILD_DATE)'

CGO_ENABLED := 1

.PHONY: build test fmt vet tidy clean install

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -ldflags "$(LDFLAGS)" -trimpath -o $(BIN_DIR)/$(BINARY) ./cmd/chronos-code

test:
	go test ./... -race -count=1

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

install:
	CGO_ENABLED=$(CGO_ENABLED) go install -ldflags "$(LDFLAGS)" -trimpath ./cmd/chronos-code
