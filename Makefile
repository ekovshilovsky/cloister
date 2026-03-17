VERSION ?= 0.1.0
BINARY  := cloister
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
