BINARY  := cloister

# Derive version from git tags. A tagged commit produces a clean semver
# (e.g. "0.0.2"); a commit ahead of a tag produces a pre-release version
# (e.g. "0.0.2-dev.25+9b7475f") following the semver 2.0 specification.
GIT_DESCRIBE := $(shell git describe --tags --always 2>/dev/null || echo v0.0.0)
VERSION      := $(shell echo $(GIT_DESCRIBE) | sed -E 's/^v//; s/-([0-9]+)-g(.+)/-dev.\1+\2/')

LDFLAGS := -s -w -X github.com/ekovshilovsky/cloister/cmd.Version=$(VERSION)

.PHONY: build test clean hooks

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

clean:
	rm -rf $(BINARY) dist/

hooks:
	git config core.hooksPath .githooks
