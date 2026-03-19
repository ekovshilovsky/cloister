BINARY  := cloister

# Derive version from git tags following semver 2.0 specification.
# Tagged commits produce clean versions (e.g., "0.0.2"); commits ahead
# of a tag produce pre-release versions (e.g., "0.0.2-dev.25+9b7475f").
# The "v" prefix on git tags is a convention — the binary version omits it.
GIT_DESCRIBE := $(shell git describe --tags --always 2>/dev/null || echo v0.0.0)
VERSION      := $(shell echo $(GIT_DESCRIBE) | sed -E 's/^v//; s/-([0-9]+)-g(.+)/-dev.\1+\2/')

LDFLAGS := -s -w -X github.com/ekovshilovsky/cloister/cmd.Version=$(VERSION)

.PHONY: build test clean hooks release

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf $(BINARY) dist/

hooks:
	git config core.hooksPath .githooks

# Print the resolved version string. Used by CI to avoid duplicating the
# version derivation logic in workflow files.
print-version:
	@echo $(VERSION)

# Cross-compile release binaries for all supported platforms.
# Called by CI — produces dist/<name>.tar.gz archives ready for upload.
release:
	@for PAIR in "darwin amd64" "darwin arm64"; do \
		OS=$$(echo $$PAIR | cut -d' ' -f1); \
		ARCH=$$(echo $$PAIR | cut -d' ' -f2); \
		DIR="cloister_$(VERSION)_$${OS}_$${ARCH}"; \
		mkdir -p "dist/$${DIR}"; \
		GOOS=$$OS GOARCH=$$ARCH go build -ldflags "$(LDFLAGS)" -o "dist/$${DIR}/cloister" .; \
		tar -czf "dist/$${DIR}.tar.gz" -C "dist/$${DIR}" cloister; \
	done
	@echo "Built release $(VERSION)"
