.SUFFIXES:

SRCS := $(wildcard *.go)

help:  ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z_.\/-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ./bin/asciiscript  ## Build the binary into ./bin (upx-compressed if available)

./bin/asciiscript: $(SRCS) makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/asciiscript .
	@if command -v upx >/dev/null 2>&1; then \
		upx ./bin/asciiscript || echo "upx failed, skipping compression"; \
	fi

.PHONY: install
install: ./bin/asciiscript  ## Symlink the binary into ~/.local/bin
	mkdir -p $(HOME)/.local/bin
	ln -sf "$(CURDIR)/bin/asciiscript" "$(HOME)/.local/bin/asciiscript"

.PHONY: clean
clean:  ## Remove build artifacts
	rm -f ./bin/asciiscript

.PHONY: test
test:  ## Run the test suite with the race detector
	go test -race ./...

.PHONY: lint
lint:  ## go vet + gofmt check (no writes)
	go vet ./...
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then \
		echo "unformatted files:"; echo "$$out"; exit 1; \
	fi

.PHONY: format
format:  ## gofmt the tree in place
	gofmt -s -w .

.PHONY: verify
verify: lint test  ## Aggregate gate: lint + test
