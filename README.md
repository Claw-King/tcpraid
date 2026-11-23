Chinese: [README_zh.md](README_zh.md)

# tcpraid

tcpraid carries **one TCP connection** over **several TCP connections** and
puts it back together, in order, at the far end. The application does not
change and does not know.

It is for software that opens only one TCP connection for a large transfer --
storage replication, a backup stream, a database's replication -- on a long or
lossy path where one connection is slow or erratic while several in parallel do
well.

The name borrows the idea behind RAID: several ordinary members that together
are faster and more robust than one.

Status: 0.1, a first public version. Wire protocol version 1 (`PROTOCOL.md`);
it may still change before 1.0. Read "Known limits" below before you rely on
it.

## Is it for you?

It helps when all of these hold:

- You have a long-lived bulk stream that cannot split itself into parallel
  connections. Examples with a tutorial here: ZFS replication on TrueNAS, an
  rsync daemon, PostgreSQL streaming replication, a Proxmox Backup Server sync
  job.
- On your path, several parallel TCP connections together are clearly faster
  or steadier than one. (A quick test: `iperf3`, a network speed test tool,
  with one stream and with eight.)
- You can run tcpraid at both ends.

It adds, on top of the speed:

- **Outages do not kill the stream.** If every sub-connection fails, both ends
  hold the application's connection for up to 15 minutes and carry on when the
  path is back.
- **A cut-off stream is not mistaken for a complete one.** An abnormal end
  resets the application's connection on both sides. That holds when a node
  is stopped, and also when its process is killed or runs out of memory: a
  node sets up the application's connection so that the system resets it if
  the process dies. (What remains is under "Known limits": an empty
  connection at the stop of a node, and a machine that loses power.)
- **Encryption between the two nodes** by default (TLS 1.3); each node checks
  the other's key against what it stored when the two were paired.

It does not help when:

- One TCP connection already fills the line.
- The path is short and clean (a LAN, a nearby data centre).
- The path is good and the one connection has socket buffers that are large
  enough: it can then be as fast as several, or faster. What tcpraid adds
  shows on a path that loses packets -- it is steadier there -- and it does
  not depend on anyone enlarging buffers.
- Something between the two nodes puts their connections into one
  connection of its own: an SSH port forward, or frp with its default
  settings. tcpraid still works there and gains nothing
  (`docs/en/connecting.md` says which tools do this and what to set).
- The application can open parallel connections itself. Use that instead.

It is not a VPN and not a general proxy: it carries the TCP connections you
make rules for, between two machines you run.

## How it is used

You run one **node** at each site -- on the NAS itself, in a container, or on
a router or gateway that serves the local network. You **pair** the two nodes
once, with a code you copy from one to the other. Then, on either node, you
make **forwarding rules**: "connections that arrive on this port go to that
address at the other site".

```
 application --> [ node B ] ===== several TCP connections =====> [ node A ] --> destination
                  rule: port 9801 -> 192.0.2.10:873 at node A's site
```

Only one of the two nodes has to be reachable from the other. Everything can
be done in the node's web page or with the `tcpraid` command.

## Five-minute start

Two machines, `nas-a` (reachable from the other at `nas-a.example.org`) and
`nas-b`. With containers, using `deploy/docker-compose.yml` on each:

1. **Start a node on each machine.** In the compose file, set `hostname:` to
   the node's name first -- `nas-a` on one machine, `nas-b` on the other: a
   node takes its name from it at its first start, and the two must differ.
   Then `docker compose up -d` and `docker logs tcpraid`: the log shows the
   password of the web page, once.
2. **Open the web page** of each node: `http://ADDRESS-OF-THE-NAS:9850/`.
3. **Pair them.** (If you did not set the names in step 1, do it now, before
   pairing: Settings > "Name of this node". A node keeps the name its peer
   had when they were paired.) On `nas-a`: Peers > "Create pairing code"; as the address
   at which the other node reaches this one enter `nas-a.example.org:9851`;
   copy the code. On `nas-b`: Peers > "Enter pairing code"; paste it. Both
   pages now list the other node as online.
4. **Make a rule.** On `nas-b`: Rules > "New rule"; listen address and port
   `0.0.0.0:50000`, peer `nas-a`, destination `192.0.2.10:873` (an address as
   `nas-a` sees it).
5. **Use it.** Point the application at `nas-b` port 50000 instead of
   `192.0.2.10` port 873.

The same with commands (here inside the containers; without containers leave
out `docker exec ... /` and `-data /data`):

```
nas-a$ docker exec tcpraid /tcpraid pair create -data /data -addr nas-a.example.org:9851
tcpraid-pair-1-iiph6pbe4iey...
nas-b$ docker exec -i tcpraid /tcpraid pair join -data /data
tcpraid-pair-1-iiph6pbe4iey...        <- pasted; then Enter and Ctrl-D
paired with nas-a (fingerprint 056f8829...e2e077d8)
nas-b$ docker exec tcpraid /tcpraid rules add rsync -data /data -peer nas-a \
           -listen 0.0.0.0:50000 -dest 192.0.2.10:873
rule rsync added: listening
```

`docs/en/install-container.md` and `docs/en/install-binary.md` go through
this slowly.

## Requirements

- **Linux at both ends** (amd64, arm64 or 32-bit ARM v7), as a container or as
  one static binary.
- **A node at each end.** A stock server on the far side is not enough.
- **One of the two nodes can open TCP connections to the other**, directly or
  through a port forward, a tunnel or a SOCKS5 proxy. Neither site needs a
  public IPv4 address for that; `docs/en/connecting.md` goes through the ways.
- **TCP application traffic only.**
- **Memory.** In the worst case a node holds about 512 MB of stream data
  (twice `-budget`, which `tcpraid run` can lower).
- **Clocks within 10 minutes of each other.**

## Documentation

| | |
|---|---|
| `docs/en/concepts.md` | nodes, pairing, peers, paths and roles, security tiers, rules, how the transport works |
| `docs/en/install-container.md` | a node in a container: compose, TrueNAS, Synology, QNAP, Unraid |
| `docs/en/install-binary.md` | a node from the binary, with systemd |
| `docs/en/connecting.md` | how the two nodes reach each other: IPv6, overlay networks, a machine in between; which tools keep the connections separate |
| `docs/en/tutorial-truenas-zfs-replication.md` | **ZFS replication between two TrueNAS systems** |
| `docs/en/tutorial-rsync.md` | rsync to an rsync daemon |
| `docs/en/tutorial-postgresql.md` | PostgreSQL streaming replication |
| `docs/en/tutorial-pbs.md` | a Proxmox Backup Server sync job |
| `docs/en/security.md` | what is protected, what is not, what a leaked code or file is worth |
| `docs/en/tunnels.md` | using it together with a tunnel, an overlay network or a proxy |
| `docs/en/troubleshooting.md` | what to look at when it does not work or is slow |
| `docs/en/reference-cli.md` | every command |
| `docs/en/reference-api.md` | the local HTTP API, with examples |
| `docs/en/reference-files.md` | the data directory and the options of `tcpraid run` |
| `docs/en/manual-mode.md` | the mode without nodes: one client, one server, flags or a file |
| `PROTOCOL.md` | the wire protocol |
| `deploy/README.md` | the build script, the image, the container templates |

## Known limits

This is a first version, made by one developer in spare time. What is known:

- **Linux only.** Other systems are not supported.
- **Tested on few machines.** The tutorials were run in containers with
  ordinary programs standing in for a NAS. After version 0.1.0 was released,
  its first user ran it on real machines: on amd64 and arm64 hosts, and in
  containers on the routers of two sites (see "Field results"). What is still
  untried: the steps of the tutorials that are done in the web pages of a
  NAS, on a real NAS of any brand; the container templates for Unraid and
  QNAP, which were only checked against the published formats.
- **The web page has been used very little.** It was rendered and clicked
  through by a script, not used by people.
- **The destination of a rule is an IP address**, not a host name.
- **Stopping a node while a destination is still being connected.** If a node
  is stopped at the very moment a session that has just begun is connecting to
  its destination at that node, the destination may see a connection that is
  opened and closed again without any data, like an empty stream.
- **A session does not survive a restart** of either node; the applications
  see a reset and have to try again. Next to the node that was stopped or
  killed, they see it at once. Next to the other node, they see it at once
  as well when the first node was stopped (it tells its peers). When it was
  killed, they see it as soon as it is back and the two are connected again,
  usually within seconds: a node that comes back says that it runs as a new
  process, and the other resets what it still held for the old one. Two
  cases wait longer, until the 15 minutes (`-grace`) are over: a node that
  does not come back, and a session that had begun only seconds (up to about
  ten) before its peer was killed. Nodes of version 0.1.1 or older neither
  say that they are new nor act on hearing it; with one of them on either
  side, a node that is killed may leave the other waiting the 15 minutes.
- **A machine that loses power or freezes** sends nothing. The applications
  next to it see no end of their connections until their own timeouts; the
  node at the other site holds its sessions as described in the previous
  point.
- **One of the two nodes has to be reachable from the other**; nothing is
  relayed through a third machine. A node that holds a path to its peer does
  not fall back to asking the peer to connect when that path fails.
- **The web page and the local API are plain HTTP.** Use them on a network you
  trust, or behind a reverse proxy with TLS.
- **A listener limits, but does not prevent,** what someone can do by holding
  many connections open against it (`docs/en/security.md`).
- **Both nodes must run the same wire protocol version**; there is no
  negotiation yet.
- **Paths through SOCKS5 and the `owncert` tier** work in the tests of the
  program but were not part of the tutorial runs or of the first use on real
  machines.

## Field results

From the author's own use of the program this one grew out of, in manual mode,
on an intercontinental path of about 240 ms, with storage replication as the
load:

- A 205 GB stream averaged 85 MB/s; about 4% of chunks were re-sent.
- A 7 TiB stream ran for five days. Its data path was down for 10 minutes on
  one occasion; the session held and continued.

These are single measurements on one path, not a benchmark, and they were not
made with this version.

Version 0.1.0 was then run by its first user, as nodes:

- Between an amd64 and an arm64 host: 8 GiB of random data, the same checksum
  at both ends.
- Between containers on the routers of two sites, over a long-distance path,
  in the `tls` tier, with one path for data and one for repairs and feedback,
  feedback over UDP as well, and the connections opened by the node the rule
  was not on: 20 GiB of random data, the same checksum at both ends, under 1%
  of it sent again.
- On those routers, an existing storage replication task, redirected to the
  node (`prerouting`) and carried by a transparent rule, ran to its end with
  nothing changed on the storage systems.

These, too, are single runs.

## Building and testing

```
go build -trimpath -o tcpraid ./cmd/tcpraid      # Go 1.27 or newer
go vet ./... && go test -race -count=1 ./...
```

`deploy/build.sh` builds the release binaries; `deploy/Dockerfile` the image.

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
