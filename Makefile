BINARY  := cloister

# Derive version from git tags following semver 2.0 specification.
# Tagged commits produce clean versions (e.g., "0.0.2"); commits ahead
# of a tag produce pre-release versions (e.g., "0.0.2-dev.25+9b7475f").
# The "v" prefix on git tags is a convention — the binary version omits it.
GIT_DESCRIBE := $(shell git describe --tags --always 2>/dev/null || echo v0.0.0)
VERSION      := $(shell echo $(GIT_DESCRIBE) | sed -E 's/^v//; s/-([0-9]+)-g(.+)/-dev.\1+\2/')

LDFLAGS := -s -w -X cloister.io/cmd.Version=$(VERSION)

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

# Build macOS release tarballs. identity_darwin.go uses cgo (libproc
# ri_proc_start_abstime), so this target must run on macOS with CGO_ENABLED=1.
# An arm64 host builds amd64 with Apple clang -arch x86_64; an x86_64 host
# builds arm64 with clang -arch arm64. A failed go build aborts the recipe
# before any tarball is written for that architecture.
release:
	@set -e; \
	rm -f dist/cloister_$(VERSION)_darwin_amd64.tar.gz dist/cloister_$(VERSION)_darwin_arm64.tar.gz; \
	for PAIR in "darwin amd64" "darwin arm64"; do \
		OS=$$(echo $$PAIR | cut -d' ' -f1); \
		ARCH=$$(echo $$PAIR | cut -d' ' -f2); \
		DIR="cloister_$(VERSION)_$${OS}_$${ARCH}"; \
		rm -rf "dist/$${DIR}"; \
		mkdir -p "dist/$${DIR}"; \
		HOST_ARCH=$$(uname -m); \
		if [ "$$ARCH" = amd64 ] && [ "$$HOST_ARCH" = arm64 ]; then \
			CC="clang -arch x86_64" CGO_ENABLED=1 GOOS=$$OS GOARCH=$$ARCH \
				go build -ldflags "$(LDFLAGS)" -o "dist/$${DIR}/cloister" .; \
		elif [ "$$ARCH" = arm64 ] && [ "$$HOST_ARCH" = x86_64 ]; then \
			CC="clang -arch arm64" CGO_ENABLED=1 GOOS=$$OS GOARCH=$$ARCH \
				go build -ldflags "$(LDFLAGS)" -o "dist/$${DIR}/cloister" .; \
		else \
			CGO_ENABLED=1 GOOS=$$OS GOARCH=$$ARCH \
				go build -ldflags "$(LDFLAGS)" -o "dist/$${DIR}/cloister" .; \
		fi; \
		test -x "dist/$${DIR}/cloister"; \
		if [ -f CHANGELOG.md ]; then cp CHANGELOG.md "dist/$${DIR}/"; fi; \
		if [ -f LICENSE ]; then cp LICENSE "dist/$${DIR}/"; fi; \
		tar -czf "dist/$${DIR}.tar.gz" -C "dist/$${DIR}" .; \
	done
	@echo "Built release $(VERSION)"

LDFLAGS_VM := -s -w -X main.Version=$(VERSION)

# Cross-compile cloister-vm release binaries for Linux targets.
# Called by CI — produces dist/<name>/ directories containing the binary,
# ready to be packaged into .deb archives by scripts/build-deb-vm.sh.
# A failed go build aborts the recipe before later architectures run.
release-vm:
	@set -e; \
	for ARCH in amd64 arm64; do \
		DIR="cloister-vm_$(VERSION)_linux_$${ARCH}"; \
		rm -rf "dist/$${DIR}"; \
		mkdir -p "dist/$${DIR}"; \
		GOOS=linux GOARCH=$$ARCH go build -ldflags "$(LDFLAGS_VM)" \
			-o "dist/$${DIR}/cloister-vm" ./cmd/cloister-vm; \
		test -x "dist/$${DIR}/cloister-vm"; \
	done
	@echo "Built cloister-vm $(VERSION)"
