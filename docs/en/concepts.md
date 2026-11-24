# Concepts

## Node

A **node** is one running `tcpraid run`. It is a long-running process with a
data directory, its own identity, a web page, a local HTTP API and, if asked
for, a listener for other nodes. You run one per site, either on the NAS itself
or on a router or gateway that serves the machines behind it.

A node's identity is a key pair made at its first start. The SHA-256 of the
public key, in hex, is the node's **fingerprint**. It stays the same for as
long as the data directory is kept.

## Pairing and peers

Two nodes are **paired** once. One of them makes a **pairing code**, which is
one long line of text, and you enter it on the other. Afterwards each node
lists the other as a **peer**, and every connection between them is
authenticated in both directions.

Pairing with a node means trusting it, because a peer's connections may go to
any address the other node can reach unless you narrow that with the peer's
allow list, described in `security.md`.

`tcpraid peers` and the Peers page show a peer as **online** while a control
connection between the two nodes is up. The control connection is a small
connection that the node with a path keeps open, with a keepalive every 10
seconds.

## Paths, roles and sub-connections

A **path** is one way for a node to reach a peer. It is an address, optionally
reached through a SOCKS5 proxy. Pairing creates one path, on the node where
the code was entered, to the address the pairing went through. A peer can have
up to 16.

For every application connection it carries, a node opens several
**sub-connections** on each path and replaces any that are lost. A path's
**roles** say what it carries:

| Role | Carries | Sub-connections |
|---|---|---|
| `data` | new data, data sent again, feedback | the path's number (8 unless changed, 1 to 64) |
| `repair` | data that has to be sent again; new data only while no `data` sub-connection is up | the path's number |
| `control` | feedback only | 1 |

`data` and `repair` cannot share a path. One path with the role `data` is what
pairing gives you and is enough to start. A second, independent path for
`repair` and `control` helps on lines where one direction loses many packets
while the other is busy, because holes are then filled over the second path
and feedback does not queue behind bulk data.

## Which node connects

Rules work on both nodes as soon as one of the two can reach the other.

- A rule on a node that holds a path to the peer opens its sub-connections
  over that path.
- A rule on a node **without** a path asks the peer, over the control
  connection, to open them from its end. The request names no address, and
  the peer dials over its own paths. From then on the connection is carried
  as in the other case.

So only one node needs a listener (`-listen`) that the other can reach, and
nothing is relayed through a third machine. If each node can reach the other,
give each a path to the other, and each rule then uses its own node's paths.
A rule can also say which node opens its sub-connections, or let both
nodes open them for the same session, with `-dial` (`reference-cli.md`).

A session whose sub-connections the peer opens begins a little later than
one the node dials for itself, because the request and the peer's connect
come first. The peer therefore keeps a few spare connections ready, and such
a session begins on one of them (`connecting.md`, "Spare connections").

While the peer is offline, a rule on the node without a path resets its
connections immediately and says why in its status.

## Security tiers of a path

| Tier | On the wire | How the ends know each other |
|---|---|---|
| `tls` (default) | TLS 1.3 | each end shows its identity certificate; the other checks the key's fingerprint against the one stored at the pairing |
| `owncert` | TLS 1.3 | the listening end shows a certificate you supply (`-cert`, `-key`). The dialing end checks it by the fingerprint of its key (`pin`) or, like a web client, by authority and host name (`ca`) |
| `off` | plain TCP | nothing at this layer; for a path that something else already encrypts. Goes to the peer's `-listen-plain` |

The tier is a property of the path and is set on the dialing node. In every
tier a connection starts with a short opening that proves, with a key derived
from the pairing, which node is dialing. The transport authenticates its own
messages the same way. `security.md` says what each tier protects.

## Forwarding rules

A **rule** tells a node which connections to carry to which peer.

A **fixed rule** listens on an address of the node. Any connection made there
comes out at the peer's end and goes to the rule's destination, which is an IP
address and port as the peer sees it. A rule can cover a **range of ports**,
mapped port by port, for example `50000-50010` to `50000-50010`.

A **transparent rule** is for a node on a router or gateway. The host
redirects connections to the node with an nftables or iptables redirect rule
that you set up yourself. tcpraid does not touch the host's packet filter. The
node reads where each connection was originally going, and a transparent rule
says which original destinations go to which peer. The connections arrive at
the same address at the peer's end. The applications keep using their normal
addresses.

Rules are stored by the node and start with it. They can be added, changed,
switched off and removed while the node runs. A change touches only that rule,
and a connection that is being carried runs to its end.

## Session

Each application connection that a node carries is a **session**. The Sessions
page, `tcpraid sessions` and `GET /api/v1/sessions` list them with their rule,
peer, byte counts and number of sub-connections. A node takes no new
connection for its rules while it has 64 sessions, and is the exit for at
most 64 at a time.

The sender can be finished before the data has arrived. A node reads ahead
from the application, up to its window (128 MB by default). So when the
sending application has written everything and closed, part of the stream may
still be in the node or on the path, and the far end goes on receiving for a
while. The session stays at the sending node until the far node has reported
all of it received. Compare files, or act on the result, when the receiving
application has finished, not when the sending one returns.

A session lives in the memory of the two nodes. If every sub-connection fails,
both nodes hold it for 15 minutes and continue when the path is back. If
either node restarts, the session is gone, and the applications see a reset
and have to try again. The applications next to the node that restarted see it
immediately, even if its process was killed. Those next to the other node
also see it immediately if the first node was stopped. If it was killed, they
see it as soon as the two nodes are connected again. Every node says, on the
control connection, which run of its process it is, and a node that hears a
different run than before resets the sessions it still holds for the old one.
If the node does not come back, the other holds its sessions for those 15
minutes. The same goes for a session that began in the last ten seconds or so
before the kill, and for a pair in which one node is of version 0.1.1 or
older. The README lists these cases under "Known limits".

## How the transport works

```
 app --> [ node ] ==== sub-connections (TCP, any path) ====> [ node ] --> destination
            ^                                                    |
            +--------------------- feedback ---------------------+
```

- **Data.** The sender cuts the stream into chunks of 256 KB and spreads them
  over the data sub-connections. Each sub-connection only holds what it
  delivers within its feedback loop plus 300 ms, so one that stalls strands
  little data.
- **Feedback.** Each end regularly sends a complete snapshot of its state:
  what it has delivered in order, which chunks it holds out of order, which
  are missing and for how long. The newest snapshot always wins, so a lost or
  late one costs nothing.
- **Loss recovery.** A chunk reported missing for more than a second is sent
  again alone, on the sub-connection that will deliver it soonest, preferably
  on another path.
- **Slow members are replaced.** A sub-connection that delivers far below the
  others on its path, or makes no progress for 20 seconds, is closed and
  dialed again.
- **Endings.** A stream that ended completely in both directions is closed
  normally. Any other end resets the application's connection, so that a
  cut-off stream cannot be taken for a complete one. That covers a node
  stopping, a node's process being killed, the end of a pairing, a
  destination that cannot be reached, and the 15 minutes running out.

### Feedback channels

Feedback travels on the data sub-connections, on a `control` path if you gave
one, and optionally as UDP datagrams. The datagrams are encrypted and
authenticated, one is sent per snapshot, and none is sent again. The first
copy to arrive is used. UDP is off unless you set it up. To do that, start the
node that has a reachable UDP port with `-udp-listen ADDR`, and set the peer's
UDP feedback address on the other node, either with
`tcpraid peers set NAME -udp-dial HOST:PORT` or under Peers > Details >
Advanced in the web page. UDP feedback helps on lines whose reverse direction
loses many packets while the forward direction is busy.

`PROTOCOL.md` has the formats.
