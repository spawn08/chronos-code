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

CGO_ENABLED ?= 1

SIZE_LIMIT  := 41943040
FULL_SIZE_LIMIT := 73400320

.PHONY: build build-core build-full build-release test lint size-check size-check-core size-check-full fmt vet tidy clean install install-core eval

build: build-full

build-full:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -tags treesitter -ldflags "$(LDFLAGS)" -trimpath -o $(BIN_DIR)/$(BINARY) ./cmd/chronos-code

build-core:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -trimpath -o $(BIN_DIR)/$(BINARY)-core ./cmd/chronos-code

build-release:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
		-ldflags="-s -w -X '$(MODULE)/internal/cli.Version=$(VERSION)' -X '$(MODULE)/internal/cli.Commit=$(COMMIT)' -X '$(MODULE)/internal/cli.BuildDate=$(BUILD_DATE)'" \
		-o $(BIN_DIR)/$(BINARY) ./cmd/chronos-code

lint:
	golangci-lint run ./...

size-check: build-release
	@SIZE=$$(stat -f%z $(BIN_DIR)/$(BINARY) 2>/dev/null || stat -c%s $(BIN_DIR)/$(BINARY)); \
	echo "Binary size: $$SIZE bytes ($$(( SIZE / 1048576 )) MB)"; \
	if [ "$$SIZE" -gt "$(SIZE_LIMIT)" ]; then \
		echo "FAIL: binary exceeds $(SIZE_LIMIT) bytes"; exit 1; \
	fi; \
	if [ "$$SIZE" -gt 31457280 ]; then \
		echo "WARN: binary exceeds 30 MB target ($(SIZE_LIMIT) byte gate active)"; \
	fi

size-check-core: build-core
	@SIZE=$$(stat -f%z $(BIN_DIR)/$(BINARY)-core 2>/dev/null || stat -c%s $(BIN_DIR)/$(BINARY)-core); \
	echo "Core binary: $$SIZE bytes (limit $(SIZE_LIMIT))"; \
	test "$$SIZE" -le "$(SIZE_LIMIT)"

size-check-full: build-full
	@SIZE=$$(stat -f%z $(BIN_DIR)/$(BINARY) 2>/dev/null || stat -c%s $(BIN_DIR)/$(BINARY)); \
	echo "Full binary: $$SIZE bytes (limit $(FULL_SIZE_LIMIT))"; \
	test "$$SIZE" -le "$(FULL_SIZE_LIMIT)"

test:
	go test ./... -race -count=1

# eval runs the token-efficiency eval suite (PRD P3-006) against the
# checked-in baseline (benchmark/eval/baseline.json) and fails if any task's
# efficiency contract broke or optimized tokens regressed >10%. It is fully
# offline/deterministic — no API key or network access required.
eval: build
	$(BIN_DIR)/$(BINARY) eval run --md benchmark/eval/report.md

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

install:
	CGO_ENABLED=$(CGO_ENABLED) go install -tags treesitter -ldflags "$(LDFLAGS)" -trimpath ./cmd/chronos-code

install-core:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" -trimpath ./cmd/chronos-code
