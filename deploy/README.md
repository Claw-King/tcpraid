# Packaging

This document says what is in this directory and how each file is used. The image
is published as `ghcr.io/claw-king/tcpraid`.

None of the templates has been tried on a real NAS. The image, the compose file and
the build script were run on a Linux machine (see below). The Unraid and QNAP
templates were only checked for being well-formed and against the vendors' published
formats.

## Ports

| Port | For |
|---|---|
| 9850 | web interface and local API (plain HTTP unless the node is started with `-api-tls`: publish it on a network you trust) |
| 9851 | connections from other nodes; open it on the node that the other one can reach |
| 50000-50010 | forwarding rules; a rule listens on one of them, at the address `0.0.0.0` |
| 8446/udp | optional: feedback from the other node over UDP. Off unless the node is started with `-udp-listen` |

The range for rules is only a default. To change it in a template, change the host and
the container side of the mapping to the same ports. With the host's network instead
of mapped ports (see the compose file), a rule can listen on any free port.

UDP feedback is optional and off by default. To use it, the node's command line needs
`-udp-listen 0.0.0.0:8446`, the port has to be published as UDP, and the other node is
told the address with `tcpraid peers set NAME -udp-dial HOST:8446`. The compose file
has both lines in it, commented out. The Unraid template publishes the port, which
does nothing until the command line of the container has `-udp-listen` as well. The
QNAP template does not publish it. Add the port and the argument there by hand.

## Files

- `build.sh`: builds the release binaries (static on Linux) with the version built in, and a
  file with their SHA-256 sums. The targets are linux/amd64, linux/arm64 and
  linux/arm v7, macOS amd64 and arm64, and Windows amd64 and arm64. Run it as
  `deploy/build.sh 0.10.1`. Its header shows how to run it without Go installed.
- `Dockerfile`: the image, which is the binary and the system's certificate
  authorities on an empty base. Build it with
  `docker build -f deploy/Dockerfile --build-arg VERSION=0.10.1 -t tcpraid:0.10.1 .`
  The default command starts a node with its data in the volume `/data`, and the
  health check is `tcpraid status`. Any other tcpraid command line can be given
  instead.
- `docker-compose.yml`: one node. It is used as it is by `docker compose up -d`, and
  wherever a platform takes Compose YAML: TrueNAS (Install via YAML), Synology
  Container Manager (Project), QNAP Container Station (Applications). Set the image,
  the data directory and, if you like, the password. The comments in the file say
  how.
- `unraid/tcpraid.xml`: a container template for Unraid, which does not take Compose
  files. Each port of the range for rules is a separate entry, because that is the
  form the template format documents. Add or remove entries in the container's form.
- `qnap/tcpraid-templates.json`: a custom app template list for QNAP Container
  Station. Go to Preferences > App Templates > Enable custom template and enter the
  URL of this file. The template then appears under App Templates > Custom Templates.
  The file has to be served from a web address the NAS can reach. It has one entry
  per processor type, amd64 and arm64. On QNAP the compose file works too
  (Applications > Create).
- `tcpraid-node.service`: a systemd unit for a node on a plain Linux host or a
  router. It runs the node as the user `tcpraid`, which its header says how to
  make, without capabilities and with much of the system out of its reach. We
  checked it with `systemd-analyze verify` and have not installed or started it
  on a machine. The unit for the manual mode is `examples/tcpraid-client.service`.

## After the first start

- The node's name is the host name of its first start with four random characters
  added, for example `tcpraid-k3x9` with the compose file or the QNAP template.
  Change it under Settings in the web interface before you pair the node.
- Without a password given, the node makes one and prints it once in its log.
- The container does not know the address other nodes reach it at. Name it when you
  make a pairing code, in "Address at which the other node reaches this one", or add
  `-advertise HOST:9851` to the command.
