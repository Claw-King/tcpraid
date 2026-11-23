#!/bin/sh
# Builds the release binaries of tcpraid: static, for linux/amd64, linux/arm64 and
# linux/arm (v7), with the version built in, and a file with their SHA-256 sums.
#
# usage: deploy/build.sh [VERSION [OUTDIR]]      (defaults: 0.1.1-dev, dist)
#
# Needs Go 1.27 or newer. Without Go on the machine, run it in the Go image, from the
# directory that holds go.mod:
#
#   docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -v "$PWD":/src -w /src \
#       golang:1.27 deploy/build.sh 0.1.1
set -eu

version=${1:-0.1.1-dev}
out=${2:-dist}

cd "$(dirname "$0")/.."
mkdir -p "$out"

build() { # build GOARCH GOARM NAME
	file="tcpraid-$version-linux-$3"
	echo "building $file"
	CGO_ENABLED=0 GOOS=linux GOARCH="$1" GOARM="$2" \
		go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$version" \
		-o "$out/$file" ./cmd/tcpraid
}

build amd64 "" amd64
build arm64 "" arm64
build arm 7 armv7

cd "$out"
sha256sum "tcpraid-$version-linux-amd64" "tcpraid-$version-linux-arm64" "tcpraid-$version-linux-armv7" \
	>"tcpraid-$version-SHA256SUMS"
cat "tcpraid-$version-SHA256SUMS"
