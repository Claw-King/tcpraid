#!/bin/sh
# Builds the release binaries of tcpraid (static on Linux), with the version built in, for
# linux/amd64, linux/arm64 and linux/arm (v7), for macOS (amd64, arm64) and for Windows
# (amd64, arm64), and a file with their SHA-256 sums.
#
# usage: deploy/build.sh [VERSION [OUTDIR]]      (defaults: 0.3.0-dev, dist)
#
# Needs Go 1.27 or newer. Without Go on the machine, run it in the Go image, from the
# directory that holds go.mod:
#
#   docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD":/src -w /src \
#       golang:1.27 deploy/build.sh 0.3.0
set -eu

version=${1:-0.3.0-dev}
out=${2:-dist}

cd "$(dirname "$0")/.."
mkdir -p "$out"

build() { # build GOOS GOARCH GOARM NAME
	file="tcpraid-$version-$4"
	echo "building $file"
	CGO_ENABLED=0 GOOS="$1" GOARCH="$2" GOARM="$3" \
		go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$version" \
		-o "$out/$file" ./cmd/tcpraid
	files="$files $file"
}

files=
build linux amd64 "" linux-amd64
build linux arm64 "" linux-arm64
build linux arm 7 linux-armv7
build darwin amd64 "" macos-amd64
build darwin arm64 "" macos-arm64
build windows amd64 "" windows-amd64.exe
build windows arm64 "" windows-arm64.exe

cd "$out"
# shellcheck disable=SC2086 # the file names have no spaces
sha256sum $files >"tcpraid-$version-SHA256SUMS"
cat "tcpraid-$version-SHA256SUMS"
