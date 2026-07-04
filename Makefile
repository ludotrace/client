.PHONY: build build-mac build-mac-arm build-windows build-linux build-all test lint clean

BINARY  := ludotrace
DIST    := dist
PKG     := ./cmd/ludotrace
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
MANIFEST_URL := https://ludotrace.com/client/version.json
LDFLAGSBASE := -X github.com/ludotrace/client/internal/version.Version=$(VERSION) \
               -X github.com/ludotrace/client/internal/updater.ManifestURL=$(MANIFEST_URL)

build:
	mkdir -p $(DIST)
	rm -f $(DIST)/$(BINARY)
	go build -ldflags "$(LDFLAGSBASE)" -o $(DIST)/$(BINARY) $(PKG)

# CGO_ENABLED=1 is explicit: systray needs CGO (Cocoa) on darwin, and Go
# disables CGO by default when GOARCH != host arch. On an arm64 macOS runner
# the amd64 target is a cross-arch build, so without this the Cocoa symbols
# drop out. Must run on a macOS host (clang + macOS SDK target both arches).
build-mac:
	mkdir -p $(DIST)
	rm -f $(DIST)/$(BINARY)-mac-x64
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGSBASE)" -o $(DIST)/$(BINARY)-mac-x64 $(PKG)

build-mac-arm:
	mkdir -p $(DIST)
	rm -f $(DIST)/$(BINARY)-mac-arm64
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGSBASE)" -o $(DIST)/$(BINARY)-mac-arm64 $(PKG)

build-windows:
	mkdir -p $(DIST)
	rm -f $(DIST)/$(BINARY).exe
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui $(LDFLAGSBASE)" -o $(DIST)/$(BINARY).exe $(PKG)

# Requires: pkg-config libgtk-3-dev libayatana-appindicator3-dev gnome-keyring
build-linux:
	mkdir -p $(DIST)
	rm -f $(DIST)/$(BINARY)-linux
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGSBASE)" -o $(DIST)/$(BINARY)-linux $(PKG)

build-all: build-mac build-mac-arm build-windows build-linux

# Requires CGo + GTK deps (see build-linux comment) — cmd/ludotrace and
# internal/tray both link systray/dialog.
test:
	go test -race ./...

lint:
	go vet ./...

clean:
	rm -rf $(DIST)
