# syntax=docker/dockerfile:1
# ABOUTME: Multi-architecture build for the Sendspin client (torque)
# ABOUTME: Use `docker buildx build` or `podman build` (they behave the same) to
# ABOUTME: cross-build for every Raspberry Pi
#
# Build for every target with docker buildx or podman — they behave the same
# here. The `golang:1.27-bookworm` base image ships NO 32-bit armv6 variant, so
# `--platform linux/arm/v6` fails no matter the builder. armv6 is instead built
# from the arm/v7 image with ARM_VARIANT=v6 to force armv6 codegen (see the
# notes below). You MUST build per-platform so each goes to its own dist dir:
#   for p in linux/amd64 linux/arm64 linux/arm/v7 linux/arm/v6; do
#     plat="$p"; extra=""
#     if [ "$p" = linux/arm/v6 ]; then
#       plat=linux/arm/v7; extra="--build-arg ARM_VARIANT=v6"
#     fi
#     docker buildx build --platform "$plat" $extra --target export \
#       --output type=local,dest="dist/${p//\//_}" .
#   done
# The same loop with `podman build` in place of `docker buildx build` works
# verbatim. Prerequisites (docker): binfmt/QEMU + a builder:
#   docker run --privileged tonistiigi/binfmt:latest --install all
#   docker buildx create --use
# Build a single target / runnable image:
#   docker buildx build --platform linux/arm64 --target export \
#       --output type=local,dest=dist .
#   docker buildx build --platform linux/arm64 --target runtime -t torque .
#
# Platform notes (same hardening as .github/workflows/client.yml):
#   - The client links libopus + miniaudio, both cgo. Inside a build cell the
#     container and its gcc are the target architecture, but an arm/v7-built
#     miniaudio would still crash on an armv6 CPU (Pi Zero / Zero W), so the
#     v6 cell builds on the arm/v7 image and forces GOARM=6 + CGO_CFLAGS
#     -march=armv6, -marm (Thumb-2, which armv6 lacks). GOARM alone only covers
#     the Go code.
#   - GOFLAGS=-tags=nolibopusfile keeps the binary free of the libopusfile
#     dependency, consistent with the rest of the build.

# ---------------------------------------------------------------------------
FROM golang:1.27-bookworm AS builder

ARG TARGETARCH
ARG TARGETVARIANT
ARG ARM_VARIANT          # set to v6 to force armv6 codegen
ARG VERSION=dev

ENV GOFLAGS=-tags=nolibopusfile \
    CGO_ENABLED=1 \
    GOOS=linux \
    GOARCH=${TARGETARCH}

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      pkg-config \
      libopus-dev \
      libasound2-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN set -eu; \
    if [ "${TARGETARCH}" = "arm" ]; then \
        if [ "${ARM_VARIANT:-$TARGETVARIANT}" = "v6" ]; then \
            export GOARM=6 CGO_CFLAGS="-march=armv6 -mfpu=vfp -mfloat-abi=hard" \
                          CGO_LDFLAGS="-march=armv6 -mfpu=vfp -mfloat-abi=hard"; \
        else \
            export GOARM=7 CGO_CFLAGS="-march=armv7-a -mfpu=vfpv3-d16 -mfloat-abi=hard" \
                          CGO_LDFLAGS="-march=armv7-a -mfpu=vfpv3-d16 -mfloat-abi=hard"; \
        fi; \
    fi; \
    go build -trimpath \
      -ldflags "-X github.com/0xk1f0/torque/internal/version.Version=${VERSION} -s -w" \
      -o /out/torque \
      ./cmd/torque

# ---------------------------------------------------------------------------
# export — empty filesystem carrying only the built binaries, so
# `--output type=local` copies just them (not the whole builder rootfs).
FROM scratch AS export
COPY --from=builder /out/ /

# ---------------------------------------------------------------------------
# runtime — optional runnable image for one platform (audio passthrough is the
# operator's responsibility; --device /dev/snd and --group-add audio typical).
FROM debian:bookworm-slim AS runtime

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      libopus0 \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/torque /usr/local/bin/torque
ENTRYPOINT ["/usr/local/bin/torque"]
