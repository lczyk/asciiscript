.SUFFIXES:

SRCS := $(filter-out version_gen.go,$(wildcard *.go))

help:  ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z_.\/-]+:.*?## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ./bin/asciiscript  ## Build the binary into ./bin (upx-compressed if available)

./bin/asciiscript: $(SRCS) generate-version makefile go.mod go.sum
	mkdir -p ./bin
	go build -o ./bin/asciiscript .
	@if command -v upx >/dev/null 2>&1; then \
		upx ./bin/asciiscript || echo "upx failed, skipping compression"; \
	fi

# Phony on purpose: version_gen.go embeds the current commit sha and dirty
# flag, so it is regenerated every build rather than dated against sources --
# which is also why SRCS excludes it.
#
# Failure is not fatal: the generator needs a git checkout, and a source tarball
# or a Docker context without .git/ has none. Without version_gen.go the
# defaults in version.go stand and --version reports 0.0.0-dev, which is the
# documented fallback -- far better than refusing to build or test at all.
.PHONY: generate-version
generate-version:  ## Regenerate version_gen.go from VERSION and the git state
	@go run github.com/lczyk/version/go/cmd/generate-version -out version_gen.go -pkg main -init \
		|| echo "couldn't stamp a version (not a git checkout?) -- carrying on unstamped"

.PHONY: install
install: ./bin/asciiscript  ## Symlink the binary into ~/.local/bin
	mkdir -p $(HOME)/.local/bin
	ln -sf "$(CURDIR)/bin/asciiscript" "$(HOME)/.local/bin/asciiscript"

.PHONY: clean
clean:  ## Remove build artifacts
	rm -f ./bin/asciiscript version_gen.go

.PHONY: test
test: generate-version  ## Run the test suite with the race detector
	go test -race ./...

# go test takes one -fuzz target at a time, hence the loop. Not part of verify:
# it explores rather than gates, and the seed corpora already run under `test`.
FUZZTIME ?= 20s

.PHONY: fuzz
fuzz: generate-version  ## Fuzz each target in turn (narrow via FUZZTIME=1m)
	@for t in $$(go test -list '^Fuzz' . | grep '^Fuzz'); do \
		echo "==> $$t ($(FUZZTIME))"; \
		go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) . || exit 1; \
	done

.PHONY: lint
lint: generate-version  ## go vet + gofmt check (no writes)
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
