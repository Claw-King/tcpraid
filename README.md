Chinese: [README_zh.md](README_zh.md)

# tcpraid

tcpraid carries **one TCP connection** over **several TCP connections** and
puts it back together, in order, at the far end. The application needs no
changes and is not aware of it.

It is for software that opens only one TCP connection for a large transfer,
such as storage replication, a backup stream or database replication. It is
meant for a long or lossy path, where one connection is slow or erratic and
several in parallel do well.

The name borrows the idea behind RAID, in which several ordinary members
together are faster and more reliable than one.

Status: 0.1, a first public version. The wire protocol is version 1
(`PROTOCOL.md`) and may still change before 1.0. Read "Known limits" below
before you rely on it.

## Is it for you?

It helps when all of these hold:

- You have a long-lived bulk stream that cannot split itself into parallel
  connections. Examples with a tutorial here: ZFS replication on TrueNAS, an
  rsync daemon, PostgreSQL streaming replication, a Proxmox Backup Server sync
  job.
- On your path, several parallel TCP connections together are clearly faster
  or steadier than one. A quick test is to run `iperf3`, a network speed test
  tool, with one stream and then with eight. Once two nodes are paired,
  `tcpraid peers test` measures the same over the nodes' own paths.
- You can run tcpraid at both ends.

It adds, on top of the speed:

- **Outages do not kill the stream.** If every sub-connection fails, both ends
  hold the application's connection for up to 15 minutes and carry on when the
  path is back.
- **A cut-off stream is not mistaken for a complete one.** An abnormal end
  resets the application's connection on both sides. That holds when a node
  is stopped, and also when its process is killed or runs out of memory,
  because each node configures the application's connection so that the
  system resets it if the process dies. Two cases remain and are listed under
  "Known limits": an empty connection at the stop of a node, and a machine
  that loses power. Versions before 0.11.0 have a third, which is listed
  there too.
- **Encryption between the two nodes** by default (TLS 1.3). Each node checks
  the other's key against the one it stored when the two were paired.

It does not help when:

- One TCP connection already fills the line.
- The path is short and clean, such as a LAN or a nearby data centre.
- The path is good and the single connection has large enough socket buffers.
  It can then be as fast as several connections, or faster. tcpraid's benefit
  shows on a path that loses packets, where it is steadier, and it does not
  depend on anyone enlarging buffers.
- Something between the two nodes merges their connections into a single
  connection, as an SSH port forward does, or frp with its default settings.
  tcpraid still works there but gains nothing. `docs/en/connecting.md` says
  which tools do this and what to set.
- The application can open parallel connections itself. Use that feature
  instead.
- The application asks for a little data, waits for it, and only then asks
  for more. Its speed is then how much it asks for at a time, divided by the
  round trip, and tcpraid can do little about that. Opening a file share at the
  other site can be like this, and `docs/en/tutorial-file-share.md` says what
  helps.

tcpraid is not a VPN or a general proxy. It carries only the TCP connections
you make rules for, between two machines you run.

## How it is used

You run one **node** at each site. It can run on the NAS itself, in a
container, or on a router or gateway that serves the local network. You
**pair** the two nodes once, with a code you copy from one to the other.
Then, on either node, you make **forwarding rules** of the form "connections
that arrive on this port go to that address at the other site".

```
 application --> [ node B ] ===== several TCP connections =====> [ node A ] --> destination
                  rule: port 9801 -> 192.0.2.10:873 at node A's site
```

Only one of the two nodes has to be reachable from the other. Everything can
be done in the node's web page or with the `tcpraid` command.

A rule with a fixed destination is the usual way in for an application.
There are three more. A transparent rule takes the connections that a
router redirects to the node. A SOCKS5 rule takes programs that speak
SOCKS5, each of which names the destination it wants. And
`tcpraid connect` puts a single connection on the standard input and output
of a command, for programs such as `ssh` that run a command to reach a
server. `docs/en/reference-cli.md` describes all of them.

For the people who run it there are four more things. `tcpraid config export`
writes the configuration of a node as one file without a secret in it, and
`tcpraid config apply` makes a node match such a file. `-metrics` serves the
node's figures to a monitoring system. `-hook` runs a program of yours when
a peer comes or goes and when a session begins or ends. And Go programs can
use a node through the package `client`.

When something goes wrong, the node says where to look. `tcpraid rules`
shows why the last connection of a rule failed, with the reason the other
node gave. `tcpraid sessions` shows whether a transfer is moving or waiting
for the line, and for how long it will wait. `tcpraid peers test` measures
what several connections carry compared with one on your line. The path test
exists only as a command and in the API, not in the web page.

## Five-minute start

The example uses two machines: `nas-a`, which the other can reach at
`nas-a.example.org`, and `nas-b`. Both run the node in a container, with
`deploy/docker-compose.yml`:

1. **Start a node on each machine.** Run `docker compose up -d` and
   `docker logs tcpraid`. The log shows the password of the web page, once.
2. **Open the web page** of each node at `http://ADDRESS-OF-THE-NAS:9850/`.
3. **Name them and pair them.** A new node is called after its host with
   four random characters added, for example `tcpraid-k3x9`. Give each node a
   name you recognise under Settings > "Name of this node", here `nas-a` and
   `nas-b`. Do it before pairing, because a node keeps the name its peer had
   when they were paired. On `nas-a`, go to Peers >
   "Create pairing code", enter `nas-a.example.org:9851` as the address at
   which the other node reaches this one, and copy the code. On `nas-b`, go
   to Peers > "Enter pairing code" and paste it. Both pages now list the
   other node as online.
4. **Make a rule.** On `nas-b`, go to Rules > "New rule". Set the listen
   address and port to `0.0.0.0:50000`, the peer to `nas-a`, and the
   destination to `192.0.2.10:873`, which is an address as `nas-a` sees it.
5. **Use it.** Point the application at `nas-b` port 50000 instead of
   `192.0.2.10` port 873.

The same with commands, shown here inside the containers. Without containers,
leave out `docker exec ... /` and `-data /data`:

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

- **Linux, macOS or Windows at each end.** The two ends need not run the same
  system. On Linux (amd64, arm64 or 32-bit ARM v7) tcpraid runs as a
  container or as one static binary. On macOS and Windows it runs as one
  binary, without the features listed under "Known limits".
- **A node at each end.** A stock server on the far side is not enough.
- **One of the two nodes can open TCP connections to the other**, directly or
  through a port forward, a tunnel or a SOCKS5 proxy. Neither site needs a
  public IPv4 address for that, and `docs/en/connecting.md` goes through the
  ways.
- **TCP application traffic only.**
- **Memory.** In the worst case a node holds about 512 MB of stream data.
  That is twice `-budget`, which `tcpraid run` can lower.
- **Clocks within 10 minutes of each other.**

## Documentation

| | |
|---|---|
| `docs/en/concepts.md` | nodes, pairing, peers, paths and roles, security tiers, rules, how the transport works |
| `docs/en/install-container.md` | a node in a container: compose, TrueNAS, Synology, QNAP, Unraid |
| `docs/en/install-binary.md` | a node from the binary: Linux (with systemd), macOS, Windows |
| `docs/en/connecting.md` | how the two nodes reach each other: IPv6, overlay networks, a forwarding host; which tools keep the connections separate |
| `docs/en/tutorial-truenas-zfs-replication.md` | **ZFS replication between two TrueNAS systems** |
| `docs/en/tutorial-rsync.md` | rsync to an rsync daemon |
| `docs/en/tutorial-postgresql.md` | PostgreSQL streaming replication |
| `docs/en/tutorial-pbs.md` | a Proxmox Backup Server sync job |
| `docs/en/tutorial-file-share.md` | the file share of the other site, and what decides its speed |
| `docs/en/security.md` | what is protected, what is not, what a leaked code or file is worth |
| `docs/en/tunnels.md` | using it together with a tunnel, an overlay network or a proxy |
| `docs/en/troubleshooting.md` | what to look at when it does not work or is slow |
| `docs/en/reference-cli.md` | every command |
| `docs/en/reference-api.md` | the local HTTP API, with examples |
| `docs/en/reference-files.md` | the data directory, the options of `tcpraid run`, the figures for monitoring, the hook, the exported configuration |
| `docs/en/go-package.md` | using a node from a Go program |
| `docs/en/manual-mode.md` | the mode without nodes: one client, one server, flags or a file |
| `PROTOCOL.md` | the wire protocol |
| `deploy/README.md` | the build script, the image, the container templates |

## Known limits

This is a first version, made by one developer in spare time. What is known:

- **macOS and Windows are new, and have fewer features.** The program was
  written and used on Linux. On macOS and Windows it has so far been run by
  the automated tests of the repository and once by hand. In that run a Mac
  (Apple silicon) and a Windows machine (amd64) on one local network were
  paired through the web page, and 1 GiB went from one to the other with the
  same checksum at both ends. These exist only on Linux: transparent rules
  (`-transparent`, `-redirect`), `-maxrss`, a socket mark on a path, the
  container image and the systemd unit.
- **Tested on few machines.** The tutorials were run in containers with
  ordinary programs standing in for a NAS. After version 0.1.0 was released,
  its first user ran it on real machines: on amd64 and arm64 hosts, and in
  containers on the routers of two sites (see "Field results"). Two things
  are still untried. The tutorial steps that are done in the web pages of a
  NAS have not been run on a real NAS of any brand, and the container
  templates for Unraid and QNAP were only checked against the published
  formats.
- **The transport settings have been used at their defaults.** Since version
  0.7.0 a node takes more of them, for the node, for one peer or for one
  rule. Other values than the defaults have been through the tests of the
  repository and through no transfer on a real line. The documents say what
  each setting changes and name no values to use.
- **The web page has been used very little.** It was rendered and clicked
  through by a script, and one person has used it to pair two nodes. What
  came with version 0.8.0, the block for the transport settings and the
  address that the form for a pairing code starts with, was run by a script
  without a browser, and nobody has looked at it in one. The same holds for
  the page served over TLS (`-api-tls`): the commands and `curl` were run
  against it, and no browser.
- **A host name as the destination of a rule needs version 0.4.0 or later on
  both nodes.** The peer looks the name up for every connection. Nothing is
  kept between two connections, so each one waits for that lookup first.
- **A reset that another call takes, in versions before 0.11.0.** The system
  reports a reset that arrives on a connection to one call only. If a write
  of the node is that call, the node's read of the same connection sees an
  ordinary end. Versions before 0.10.1 could pass such a reset on as the end
  of a complete stream. In a test written to provoke this, a few of every
  hundred such connections ended that way. Since version 0.10.1 the node
  allows for it where it writes data to an application and where it answers
  a SOCKS5 client or a `connect` request, and since version 0.11.0 also where
  it ends its sending side on a connection to the API under `-api-tls`,
  which is a write too.
- **Stopping a node while a destination is still being connected.** If a node
  is stopped at the very moment a session that has just begun is connecting to
  its destination at that node, the destination may see a connection that is
  opened and closed again without any data, like an empty stream.
- **A session does not survive a restart** of either node. The applications
  see a reset and have to try again. The applications next to the node that
  was stopped or killed see it immediately. Those next to the other node also
  see it immediately if the first node was stopped, because a stopping node
  tells its peers. If it was killed, they see it as soon as it is back and
  the two are connected again, usually within seconds. A node that comes back
  says that it runs as a new process, and the other node resets what it still
  held for the old one. A node that does not come back leaves the other
  waiting until the 15 minutes (`-grace`) are over. So does a session that
  had begun only seconds (up to about ten) before its peer was killed, unless
  both nodes run version 0.11.0 or later: such nodes ask the new process which
  of those sessions it holds, and reset the others. Nodes of version 0.1.1 or
  older neither say that they are new nor act on hearing it. With one of them
  on either side, a node that is killed may leave the other waiting the 15
  minutes.
- **A machine that loses power or freezes** sends nothing. The applications
  next to it see no end of their connections until their own timeouts. The
  node at the other site holds its sessions as described in the previous
  point.
- **One of the two nodes has to be reachable from the other.** Nothing is
  relayed through a third machine. A path can name several addresses, which
  are tried in order. A node whose path to its peer fails asks the peer to
  open the connections only when the peer holds a path to it too. That
  change takes from a few seconds to about a minute, and sessions that are
  running are not moved. A rule with `-dial both` has sub-connections from
  both nodes in one session, which goes on over those of one node while the
  path of the other is down (`docs/en/connecting.md`).
- **The systemd unit was checked by `systemd-analyze` only.** It runs the node
  as a separate user with a set of restrictions, and it has not been
  installed or started on a machine.
- **The web page and the local API are plain HTTP unless the node is started
  with `-api-tls`.** Without it, use them on a network you trust, or behind a
  reverse proxy with TLS. With it, the node serves them with a certificate it
  made for itself, which a browser does not know and asks about
  (`docs/en/security.md`).
- **A listener limits, but does not prevent,** what someone can do by holding
  many connections open against it (`docs/en/security.md`).
- **There is one version of the wire protocol.** Two nodes say to each other
  which versions they speak and what they can do beyond the first release,
  and use only what both name (`PROTOCOL.md`). A node carries nothing to or
  from a peer that lists no version it speaks, and `tcpraid peers` says so.
  All releases so far speak version 1, so that case has only been run in
  tests.
- **Three things need version 0.11.0 on both nodes.** They are the answer
  "connected" of a SOCKS5 rule and of `tcpraid connect` that waits until the
  other node has reached the destination, the SOCKS5 reply for a destination
  that refuses, and the reset of a session that began seconds before its
  peer was killed. With an older node at the other end, a node tells the
  client that the connection began 150 milliseconds after the other node
  took it, and a destination that does not answer at all shows as a
  connection that began and is then reset. The exit status of
  `tcpraid connect` that says whether the whole input was passed on needs
  version 0.11.0 on the node next to the command only.
- **The word "connected" can be lost.** The other node says once, on the
  control connection, that it has reached the destination. If that
  connection ends at that very moment, the word does not arrive, and a
  client whose protocol waits for the answer before it sends anything is
  told after 12 seconds that the connection did not begin, although the
  destination was reached. A destination that speaks first is not affected.
- **Exit status 0 of `tcpraid connect` says where the input got to, not that
  it was read.** It says that the other node wrote the whole input to its
  connection to the destination and ended that direction in order. Whether
  the destination's program read it is beyond what a node can see. With an
  older node at the other end, status 0 could say less than that. A node of
  version 0.11.0 that was killed, and a node of version 0.11.0 or 0.11.1
  that was stopped, within about a second after it wrote the last of the
  input, reset its connection to the destination, and what the destination
  had not taken yet was lost. Since version 0.11.2 the system delivers the
  rest in both cases. That needs the network of the node's host to stay up
  after the node has gone. A node that is the main process of a container
  whose network goes with it, as at `docker stop` of a container on a
  network of the container's own, can still lose that rest, and the
  destination then sees neither an end nor a reset. A node that is stopped
  in that second also tells the other node that the session was aborted, so
  the command can end with status 4 although the destination gets
  everything. The command ends about a second after the two directions
  have, because it waits for the node's word. SOCKS5 rules and `tcpraid connect` were run in
  containers, with `curl` as the SOCKS5 client.
- **The exported configuration is not a backup.** It holds no key and no
  pair secret, so it cannot bring a node back or pair one. Keep the data
  directory for that.
- **A hook does not see every event.** The node runs at most four at a time
  and does not wait. Events that come faster than the hook returns are left
  out and counted.
- **The Go package `client` may still change** before version 1.0.
- **The redirect rules that a node installs itself (`-redirect`)** were run
  in containers with nftables 1.0.9, for IPv4. The container image holds no
  `nft`, so the image cannot install them. A node that is killed leaves its
  table behind until it runs again.
- **Paths through SOCKS5 and the `owncert` tier** work in the tests of the
  program but were not part of the tutorial runs or of the first use on real
  machines.

## Field results

From the author's own use of the program this one grew out of, in manual mode,
on an intercontinental path of about 240 ms, with storage replication as the
load:

- A 205 GB stream averaged 85 MB/s, and about 4% of chunks were re-sent.
- A 7 TiB stream ran for five days. Its data path was down for 10 minutes on
  one occasion, and the session held and continued.

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

These are single runs as well.

## Building and testing

```
go build -trimpath -o tcpraid ./cmd/tcpraid      # Go 1.27 or newer
go vet ./... && go test -race -count=1 ./...
```

`deploy/build.sh` builds the release binaries, and `deploy/Dockerfile` builds
the image.

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
