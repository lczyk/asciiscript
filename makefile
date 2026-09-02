.SUFFIXES:

SRCS := $(wildcard *.go)

help:  ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z_.\/-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ./bin/asciiscript  ## Build the binary into ./bin

./bin/asciiscript: $(SRCS) makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/asciiscript .

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

# go test takes one -fuzz target at a time, hence the loop. Not part of verify:
# it explores rather than gates, and the seed corpora already run under `test`.
FUZZTIME ?= 20s

.PHONY: fuzz
fuzz:  ## Fuzz each target in turn (narrow via FUZZTIME=1m)
	@for t in $$(go test -list '^Fuzz' . | grep '^Fuzz'); do \
		echo "==> $$t ($(FUZZTIME))"; \
		go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) . || exit 1; \
	done

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
