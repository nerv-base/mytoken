# MyToken!!!!! developer entry points.
#
# The app is a native-UI MyGo project: its bundle metadata lives in mygo.json
# and the release artifacts come from `go tool mygo build`. MyGo is pinned by
# go.mod's tool directive, so no global install is needed; everything below
# only drives the plain Go toolchain.

SHELL := /bin/sh
GO    ?= go

PKG     := github.com/nerv-base/mytoken
BINARY  ?= mytoken
OUT     ?= build

# The release version is the tag; without one, fall back to the version
# committed in mygo.json (what `go tool mygo build` uses on its own).
VERSION ?= $(shell git describe --tags --match 'v*' --abbrev=0 2>/dev/null | sed 's/^v//')
ifeq ($(strip $(VERSION)),)
VERSION := $(shell sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' mygo.json)
endif

LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)

# The app is pure Go: no cgo, on every platform.
export CGO_ENABLED := 0

# The first Developer ID Application certificate in the keychain, if any.
# `make dmg` signs and notarizes with it; on a machine without one, use
# `make dmg-local`. See docs/SIGNING.md.
SIGN_IDENTITY ?= $(shell security find-identity -v -p codesigning 2>/dev/null | sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -n 1)
MYGO_SIGN = $(if $(SIGN_IDENTITY),-sign '$(SIGN_IDENTITY)',-sign -)

.PHONY: all help build run test vet fmt fmt-check check app dmg dmg-local linux windows \
	signing-check notary-credentials clean version bump

all: check build

help:
	@echo "make build        build ./$(BINARY) for this machine (version $(VERSION))"
	@echo "make run          run the app from source"
	@echo "make check        gofmt check, go vet and go test"
	@echo "make app          package the app for this platform (signed if a Developer ID exists, never notarized)"
	@echo "make dmg          package, sign and notarize the universal macOS .app and .dmg (release)"
	@echo "make dmg-local    package the universal macOS .app and .dmg ad hoc, no notarization"
	@echo "make signing-check    check the Developer ID certificate and notary credentials"
	@echo "make notary-credentials  store notarytool credentials for mygo.json's keychainProfile"
	@echo "make linux        package the Linux binaries, .deb and tarballs"
	@echo "make windows      package the Windows .exe and installer (needs makensis)"
	@echo "make version      print the version that would be built"
	@echo "make bump TAG=v1.2.0  write a tag's version into mygo.json"
	@echo "make clean        remove build outputs"

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

run:
	$(GO) run -ldflags '$(LDFLAGS)' .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

check: fmt-check vet test

# --- packaging (go tool mygo build, configured by mygo.json) ----------------

app:
	$(GO) tool mygo build -skip-notarize $(MYGO_SIGN)

# The release build. Needs a Developer ID Application certificate in the
# keychain and notary credentials for mygo.json's keychainProfile; the
# preflight stops before a full build when either is missing.
dmg:
	@scripts/macos-signing check >/dev/null 2>&1 || { \
		echo "cannot sign and notarize yet:"; scripts/macos-signing check; \
		echo; echo "or build unsigned with 'make dmg-local'"; exit 1; \
	}
	$(GO) tool mygo build -platform darwin/universal -sign '$(SIGN_IDENTITY)'

dmg-local:
	$(GO) tool mygo build -platform darwin/universal -sign - -skip-notarize

signing-check:
	@scripts/macos-signing check

notary-credentials:
	@scripts/macos-signing credentials

linux:
	$(GO) tool mygo build -platform linux/amd64,linux/arm64

windows:
	$(GO) tool mygo build -platform windows/amd64,windows/arm64

# --- release helpers -------------------------------------------------------

version:
	@$(GO) run ./scripts/setversion --print

# make bump TAG=v1.2.0 — keeps mygo.json in step with the tag being pushed.
bump:
	@test -n "$(TAG)" || { echo "usage: make bump TAG=v1.2.0"; exit 2; }
	@$(GO) run ./scripts/setversion "$(TAG)"

clean:
	rm -rf $(OUT) $(BINARY)
