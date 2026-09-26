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

# No package uses cgo; builds are pure Go on every platform.
CGO_ENABLED ?= 0

# Tree-sitter grammars embedded in the binary: one per language pack under
# internal/indexer/extract/packs. Without these tags gotreesitter embeds all
# of its ~200 grammars (about 18 MiB instead of 5 MiB); the build still works.
GRAMMARS := bash c cpp c_sharp dart java javascript kotlin objc php python ruby rust scala swift tsx typescript
GRAMMAR_TAGS := grammar_subset $(addprefix grammar_subset_,$(GRAMMARS))

SIZE_LIMIT  := 58720256

.PHONY: build build-core build-full build-release grammar-tags test lint size-check size-check-core fmt vet tidy clean install install-core eval eval-edges bench-index

build: build-full

build-full:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -tags "$(GRAMMAR_TAGS)" -ldflags "$(LDFLAGS)" -trimpath -o $(BIN_DIR)/$(BINARY) ./cmd/chronos-code

build-core:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -tags "$(GRAMMAR_TAGS)" -ldflags "$(LDFLAGS)" -trimpath -o $(BIN_DIR)/$(BINARY)-core ./cmd/chronos-code

build-release:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "$(GRAMMAR_TAGS)" -trimpath \
		-ldflags="-s -w -X '$(MODULE)/internal/cli.Version=$(VERSION)' -X '$(MODULE)/internal/cli.Commit=$(COMMIT)' -X '$(MODULE)/internal/cli.BuildDate=$(BUILD_DATE)'" \
		-o $(BIN_DIR)/$(BINARY) ./cmd/chronos-code

grammar-tags:
	@echo $(GRAMMAR_TAGS)

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

test:
	go test -tags "$(GRAMMAR_TAGS)" ./... -race -count=1

# eval runs the token-efficiency eval suite (PRD P3-006) against the
# checked-in baseline (benchmark/eval/baseline.json) and fails if any task's
# efficiency contract broke or optimized tokens regressed >10%. It is fully
# offline/deterministic — no API key or network access required.
eval: build
	$(BIN_DIR)/$(BINARY) eval run --md benchmark/eval/report.md

# eval-edges measures reference resolution against SCIP indexes of pinned
# repositories (benchmark/edges/repos.tsv) and updates
# benchmark/edges/baseline.json. It needs network access and the SCIP
# indexers (see benchmark/edges/run.sh); it is not part of CI.
eval-edges:
	benchmark/edges/run.sh

# bench-index measures indexing latency of the chronos indexer on a private
# copy of this repo, and the graph tools' query latency over its index
# (docs/chronos-indexer.md, "Measurement"). Indexing iterations are
# expensive, so they run a fixed count; queries use -benchtime.
BENCH_COUNT ?= 5
bench-index:
	go test ./internal/indexer -tags "$(GRAMMAR_TAGS)" -run '^$$' -bench '^BenchmarkIndex' -benchtime=20x -count=$(BENCH_COUNT) -timeout 30m
	go test ./internal/graph -tags "$(GRAMMAR_TAGS)" -run '^$$' -bench '^BenchmarkScope' -benchtime=1s -count=$(BENCH_COUNT) -benchmem -timeout 30m

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN_DIR)

install:
	CGO_ENABLED=$(CGO_ENABLED) go install -tags "$(GRAMMAR_TAGS)" -ldflags "$(LDFLAGS)" -trimpath ./cmd/chronos-code

install-core:
	CGO_ENABLED=0 go install -tags "$(GRAMMAR_TAGS)" -ldflags "$(LDFLAGS)" -trimpath ./cmd/chronos-code
