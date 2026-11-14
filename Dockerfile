# syntax=docker/dockerfile:1
# ABOUTME: Multi-architecture build for the Sendspin client (torque)
# ABOUTME: Use `docker buildx --platform` to cross-build for every Raspberry Pi
# ABOUTME: architecture (armv6, armv7, arm64) plus desktop x86_64.
#
# Build binaries for all targets in one shot (docker buildx):
#   docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 \
#       --target export --output type=local,dest=dist .
#     produces dist/linux_amd64/torque, dist/linux_arm64/torque,
#     dist/linux_arm_v7/torque, dist/linux_arm_v6/torque.
#   (Requires binfmt/QEMU registration + a buildx builder; see below.)
#
# Build just one target (or a runnable image via --target runtime):
#   docker buildx build --platform linux/arm64 --target export \
#       --output type=local,dest=dist .
#   docker buildx build --platform linux/arm64 --target runtime -t torque .
#
# The same matrix with podman (per platform; set ${p//\//_} = linux_arm64, ...):
#   for p in linux/amd64 linux/arm64 linux/arm/v7 linux/arm/v6; do
#     plat="$p"; extra=""
#     if [ "$p" = linux/arm/v6 ]; then
#       # buildx sets TARGETVARIANT=v6 from --platform; podman does not. Build
#       # the arm/v7 cell and force armv6 codegen with ARM_VARIANT so the
#       # cgo audio stack (libopus/miniaudio) matches the Pi Zero CPU.
#       plat=linux/arm/v7; extra="--build-arg ARM_VARIANT=v6"
#     fi
#     podman build --platform "$plat" $extra --target export \
#       --output type=local,dest="dist/${p//\//_}" .
#   done
#
# Prerequisites (docker): cross-platform builds need QEMU/binfmt registration:
#   docker run --privileged tonistiigi/binfmt:latest --install all
#   docker buildx create --use
#
# Platform notes (same hardening as .github/workflows/v2-client.yml):
#   - The client links libopus + miniaudio, both cgo. Inside a buildx cell the
#     container and its gcc are the target architecture, but an arm/v7-built
#     miniaudio would still crash on an armv6 CPU (Pi Zero / Zero W), so armv6
#     also forces CGO_CFLAGS=-march=armv6. GOARM alone only covers the Go code.
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
