# Packaging

What is here, and how each file is used. The image is published as
`ghcr.io/claw-king/tcpraid`.

None of the templates has been tried on a real NAS. The image, the compose file and
the build script were run on a Linux machine (see below); the Unraid and QNAP
templates were only checked for being well-formed and against the vendors' published
formats.

## Ports

| Port | For |
|---|---|
| 9850 | web interface and local API (plain HTTP: publish it on a network you trust) |
| 9851 | connections from other nodes; open it on the node that the other one can reach |
| 50000-50010 | forwarding rules; a rule listens on one of them, at the address `0.0.0.0` |

The range for rules is only a default. To change it in a template, change the host and
the container side of the mapping to the same ports. With the host's network instead
of mapped ports (see the compose file), a rule can listen on any free port.

## Files

- `build.sh` -- builds the three static release binaries (linux/amd64, linux/arm64,
  linux/arm v7) with the version built in, and a file with their SHA-256 sums:
  `deploy/build.sh 0.1.2`. Its header shows how to run it without Go installed.
- `Dockerfile` -- the image: the binary and the system's certificate authorities on an
  empty base. `docker build -f deploy/Dockerfile --build-arg VERSION=0.1.2 -t tcpraid:0.1.2 .`
  The default command starts a node with its data in the volume `/data`; the health
  check is `tcpraid status`. Any other tcpraid command line can be given instead.
- `docker-compose.yml` -- one node. Used as it is by `docker compose up -d`, and where
  a platform takes Compose YAML: TrueNAS (Install via YAML), Synology Container Manager
  (Project), QNAP Container Station (Applications). Set the image, the data directory
  and, if you like, the password; the comments in the file say how.
- `unraid/tcpraid.xml` -- a container template for Unraid, which does not take Compose
  files. Each port of the range for rules is an entry of its own, because that is the
  form the template format documents; add or remove entries in the container's form.
- `qnap/tcpraid-templates.json` -- a custom app template list for QNAP Container
  Station: Preferences > App Templates > Enable custom template, then the URL of this
  file; the template appears under App Templates > Custom Templates. The file has to
  be served from a web address the NAS can reach. One entry per processor type, amd64
  and arm64. On QNAP the compose file works too (Applications > Create).
- `tcpraid-node.service` -- a systemd unit for a node on a plain Linux host or a
  router. The unit for the manual mode is `examples/tcpraid-client.service`.

## After the first start

- The node's name is the host name of its first start (`tcpraid` in the compose file
  and the QNAP template; a generated one on Unraid). Change it under Settings in the
  web interface.
- Without a password given, the node makes one and prints it once in its log.
- The container does not know the address other nodes reach it at: name it when you
  make a pairing code ("Address at which the other node reaches this one"), or add
  `-advertise HOST:9851` to the command.
