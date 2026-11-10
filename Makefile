# ABOUTME: Build automation for the Sendspin client
# ABOUTME: Provides targets for building, testing, and installing the client

.PHONY: all build build-cross test test-verbose test-coverage lint clean install help

# Version from git tag or default
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X github.com/0xk1f0/spinsync/internal/version.Version=$(VERSION)

# Build with -tags=nolibopusfile so gopkg.in/hraban/opus.v2 doesn't link
# libopusfile at runtime. The client never uses the opus.Stream API.
BUILDTAGS ?= nolibopusfile
export GOFLAGS = -tags=$(BUILDTAGS)

# Default target
all: build

# Build the client for the host platform
build:
	@echo "Building spinsync..."
	go build -ldflags "$(LDFLAGS)" -o spinsync ./cmd/spinsync

# Cross-build for Linux on every Raspberry Pi architecture + x86_64 (the same
# matrix as .github/workflows/v2-client.yml). Optional; Docker is the
# canonical way (see Dockerfile).
build-cross:
	@mkdir -p bin
	@for target in "amd64:" "arm64:" "arm:7" "arm:6"; do \
		arch="$${target%%:*}"; arm="$${target##*:}"; \
		case "$$arch" in \
		arm) cc="arm-linux-gnueabihf-gcc"; flags="-march=armv7-a -mfpu=vfpv3-d16 -mfloat-abi=hard"; \
		     [ "$$arm" = "6" ] && flags="-march=armv6 -mfpu=vfp -mfloat-abi=hard"; \
		     out="spinsync-linux-arm$$arm"; \
		     echo "  $$out ($$flags)"; \
		     GOOS=linux GOARCH=arm GOARM=$$arm CC=$$cc \
		       CGO_CFLAGS="$$flags" CGO_LDFLAGS="$$flags" \
		       PKG_CONFIG_PATH=/usr/lib/arm-linux-gnueabihf/pkgconfig \
		       go build -ldflags "$(LDFLAGS)" -o bin/$$out ./cmd/spinsync ;; \
		arm64) echo "  spinsync-linux-arm64"; \
		       GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		       PKG_CONFIG_PATH=/usr/lib/aarch64-linux-gnu/pkgconfig \
		       go build -ldflags "$(LDFLAGS)" -o bin/spinsync-linux-arm64 ./cmd/spinsync ;; \
		*) echo "  spinsync-linux-amd64"; \
		   GOOS=linux GOARCH=amd64 \
		     go build -ldflags "$(LDFLAGS)" -o bin/spinsync-linux-amd64 ./cmd/spinsync ;; \
		esac; \
	done

# Run tests
test:
	@echo "Running tests..."
	go test ./...

test-verbose:
	@echo "Running tests (verbose)..."
	go test -v ./...

# Run tests with coverage
test-coverage:
	@echo "Running tests with coverage..."
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run linter
lint:
	@echo "Running golangci-lint..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed. Install: https://golangci-lint.run/usage/install/" && exit 1)
	golangci-lint run --timeout=5m

# Clean built binaries and artifacts
clean:
	@echo "Cleaning binaries and artifacts..."
	rm -f spinsync
	rm -rf bin/
	rm -f coverage.out coverage.html

# Install the client to GOPATH/bin
install:
	@echo "Installing spinsync..."
	go install -ldflags "$(LDFLAGS)" ./cmd/spinsync

# Install spinsync as a systemd service (Linux, requires root)
install-daemon: build
	@echo "Installing spinsync service..."
	install -m 755 spinsync /usr/local/bin/spinsync
	install -m 644 dist/systemd/spinsync.service /etc/systemd/system/spinsync.service
	@if [ ! -f /etc/default/spinsync ]; then \
		install -m 644 dist/systemd/spinsync.env /etc/default/spinsync; \
		echo "Created /etc/default/spinsync — edit this file to configure."; \
	else \
		echo "/etc/default/spinsync already exists, not overwriting."; \
	fi
	systemctl daemon-reload
	@echo ""
	@echo "Installed. To start:"
	@echo "  sudo systemctl enable --now spinsync"
	@echo ""
	@echo "Configure via /etc/default/spinsync"

# Remove the spinsync systemd service
uninstall-daemon:
	@echo "Removing spinsync service..."
	-systemctl stop spinsync 2>/dev/null
	-systemctl disable spinsync 2>/dev/null
	rm -f /etc/systemd/system/spinsync.service
	rm -f /usr/local/bin/spinsync
	systemctl daemon-reload
	@echo "Removed. /etc/default/spinsync left in place (manual cleanup if desired)."

help:
	@echo "Sendspin Client - Build Targets"
	@echo ""
	@echo "  make              - Build spinsync"
	@echo "  make build-cross  - Cross-build for amd64/arm64/armv7/armv6 (needs cross toolchains)"
	@echo "  make test         - Run tests"
	@echo "  make test-verbose - Run tests with verbose output"
	@echo "  make test-coverage- Run tests with coverage report"
	@echo "  make lint         - Run golangci-lint"
	@echo "  make clean        - Remove built binaries"
	@echo "  make install      - Install to GOPATH/bin"
	@echo "  make install-daemon   - Install as a systemd service (Linux, requires root)"
	@echo "  make uninstall-daemon - Remove the systemd service"
	@echo ""
	@echo "Docker (multi-arch): docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 --target export --output type=local,dest=dist ."
	@echo ""
	@echo "Version: $(VERSION)"
