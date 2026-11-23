# The tcpraid image: the static binary, the system's certificate authorities and the
# licence texts, nothing else. No key, token or data is in it; a node keeps those in the
# volume at /data.
#
# Build, from the directory that holds go.mod:
#
#   docker build -f deploy/Dockerfile --build-arg VERSION=0.1.1 -t tcpraid:0.1.1 .
#
# The default command starts a node. Any other tcpraid command line can be given in its
# place, the manual mode included (docker run ... tcpraid:0.1.1 -mode server ...).
FROM golang:1.27 AS build
ARG VERSION=0.1.1-dev
# Set by "docker buildx build --platform ..."; empty with the classic builder, which
# builds for the machine it runs on.
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN arm="${TARGETVARIANT#v}" && \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH:-$(go env GOARCH)}" GOARM="${arm:-7}" \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" -o /out/tcpraid ./cmd/tcpraid
# an empty directory to copy: the place where the volume is mounted has to exist
RUN mkdir /out/data

FROM scratch
# the authorities are needed by a path of the owncert tier that verifies the standard way
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tcpraid /tcpraid
COPY LICENSE NOTICE /
COPY --from=build /out/data /data
VOLUME /data
# 9850: the local API and the web interface. 9851: connections from other nodes.
EXPOSE 9850 9851
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/tcpraid", "status", "-data", "/data"]
ENTRYPOINT ["/tcpraid"]
CMD ["run", "-data", "/data", "-api", "0.0.0.0:9850", "-listen", "0.0.0.0:9851"]
