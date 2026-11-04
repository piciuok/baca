# baca — macOS needs cgo (menu bar icon), Linux builds pure Go (DBus tray).
BIN      := baca
DIST     := dist
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REPO     ?= piciuok/baca
MACMIN   := CGO_CFLAGS="-O2 -g -mmacosx-version-min=13.0" CGO_LDFLAGS="-mmacosx-version-min=13.0"
LDFLAGS  := -s -w -X main.version=$(VERSION) -X baca/internal/update.Repo=$(REPO)

.PHONY: build test vet install dist clean run

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) .

test:
	go test ./...

vet:
	go vet ./...
	GOOS=linux CGO_ENABLED=0 go vet ./...

# UI server without the tray icon, from the working tree
run:
	go run . serve -open

# Copies the binary to ~/.local/bin and enables start at login.
install: build
	./bin/$(BIN) install

# Release binaries. macOS needs cgo (Xcode command line tools), so build the darwin ones on a Mac.
dist: clean
	mkdir -p $(DIST)
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-arm64 .
	@if [ "$$(uname)" = Darwin ]; then \
	  $(MACMIN) GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-darwin-arm64 . && \
	  $(MACMIN) GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-darwin-amd64 .; \
	fi
	ls -l $(DIST)

clean:
	rm -rf bin $(DIST)
