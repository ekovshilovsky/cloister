BINARY  := cloister

# Derive version from git tags following semver 2.0 specification.
# Tagged commits produce clean versions (e.g., "0.0.2"); commits ahead
# of a tag produce pre-release versions (e.g., "0.0.2-dev.25+9b7475f").
# The "v" prefix on git tags is a convention — the binary version omits it.
GIT_DESCRIBE := $(shell git describe --tags --always 2>/dev/null || echo v0.0.0)
VERSION      := $(shell echo $(GIT_DESCRIBE) | sed -E 's/^v//; s/-([0-9]+)-g(.+)/-dev.\1+\2/')

LDFLAGS := -s -w -X github.com/ekovshilovsky/cloister/cmd.Version=$(VERSION)

.PHONY: build test clean hooks release release-vm

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
# Each tarball includes the binary, CHANGELOG.md, and LICENSE so that
# users who download the release have the full changelog without needing
# access to the GitHub repository.
release:
	@for PAIR in "darwin amd64" "darwin arm64"; do \
		OS=$$(echo $$PAIR | cut -d' ' -f1); \
		ARCH=$$(echo $$PAIR | cut -d' ' -f2); \
		DIR="cloister_$(VERSION)_$${OS}_$${ARCH}"; \
		mkdir -p "dist/$${DIR}"; \
		GOOS=$$OS GOARCH=$$ARCH go build -ldflags "$(LDFLAGS)" -o "dist/$${DIR}/cloister" .; \
		[ -f CHANGELOG.md ] && cp CHANGELOG.md "dist/$${DIR}/" || true; \
		[ -f LICENSE ] && cp LICENSE "dist/$${DIR}/" || true; \
		tar -czf "dist/$${DIR}.tar.gz" -C "dist/$${DIR}" .; \
	done
	@echo "Built release $(VERSION)"

LDFLAGS_VM := -s -w -X main.Version=$(VERSION)

# Cross-compile cloister-vm release binaries for Linux targets.
# Called by CI — produces dist/<name>/ directories containing the binary,
# ready to be packaged into .deb archives by scripts/build-deb-vm.sh.
release-vm:
	@for ARCH in amd64 arm64; do \
		DIR="cloister-vm_$(VERSION)_linux_$${ARCH}"; \
		mkdir -p "dist/$${DIR}"; \
		GOOS=linux GOARCH=$$ARCH go build -ldflags "$(LDFLAGS_VM)" \
			-o "dist/$${DIR}/cloister-vm" ./cmd/cloister-vm; \
	done
	@echo "Built cloister-vm $(VERSION)"
