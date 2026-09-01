# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the cached module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off: the SRT stack is pure Go, so the binary has no native
# dependencies and drops into any base image unchanged.
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/srtbench ./cmd/srtbench

# ---- runtime ----------------------------------------------------------------
# srtbench shells out to ffmpeg for encoding, decoding and VMAF, so the runtime
# image is chosen by what its ffmpeg can do rather than by size.
#
# This one was picked by testing, not assumption. Neither Alpine 3.21 (ffmpeg
# 6.1), Debian bookworm (5.1) nor Debian trixie (7.1) ships libvmaf, so on any
# of those the full-reference pass would be silently unavailable -- ffmpeg would
# simply report an unknown filter at the moment you needed a ground truth. This
# image carries libsrt AND libvmaf, and its libvmaf was verified to actually run
# (the model is embedded in libvmaf.so, so there is no model file to mount).
FROM linuxserver/ffmpeg:9.0-cli-ls79

COPY --from=build /out/srtbench /usr/local/bin/srtbench

# srtbench binds or dials UDP and writes to stdout or an HTTP endpoint. None of
# that needs privilege, so it does not run as root.
RUN useradd --system --create-home --uid 10001 srtbench
USER 10001

WORKDIR /work

# The default SRT listen port, matching the shipped configuration.
EXPOSE 8890/udp

# Replaces the base image's ffmpeg entrypoint.
ENTRYPOINT ["/usr/local/bin/srtbench"]
CMD ["--help"]
