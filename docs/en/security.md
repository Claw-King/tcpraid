# Security

This document says what tcpraid protects, what it does not protect, and what
each secret is worth. It is about nodes (`tcpraid run`). Manual mode has its
own section in `manual-mode.md`.

## In one paragraph

Two paired nodes know each other by their keys. By default every connection
between them is TLS 1.3, authenticated in both directions against the
fingerprints stored at the pairing. What a node carries is plain again between
the application and its node, and between the far node and the destination.
A paired node is trusted fully unless you restrict it. The web page and the
local API are plain HTTP and are guarded by a password and a token.

## What is on the wire

| Stretch | Protection |
|---|---|
| application to its node | none from tcpraid: whatever the application does |
| node to node, tier `tls` (default) | TLS 1.3; each end checks the other's key against the fingerprint from the pairing |
| node to node, tier `owncert` | TLS 1.3; the listening node shows a certificate you supply, checked by the fingerprint of its key or by authority and host name; the dialing node shows its identity certificate |
| node to node, tier `off` | none: plain TCP. Only an authenticated opening and authenticated transport messages |
| far node to destination | none from tcpraid |

Use `off` only on a path that something else encrypts from node to node, such
as a VPN or an encrypted proxy chain. In that tier the opening is the only
proof of who dials, and an opening that someone recorded on the path could be
sent again within ten minutes.

In every tier, a connection between two nodes starts with a short opening. It
holds the dialing node's fingerprint, the time and a random number, under a
MAC (HMAC-SHA256) with a key derived from the pair secret. The listening node
closes, without an answer, a connection whose opening it cannot verify or
whose time is more than ten minutes from its own clock. The transport then
authenticates its own headers and feedback with another key derived from the
pair secret. No two pairs of nodes share these keys.

Everything is from the standard library of Go: TLS 1.3, X.509, Ed25519,
HMAC-SHA256, AES-256-GCM for UDP feedback. There is no cryptography of
tcpraid's own design.

## Pairing

A pairing code holds the address to connect to, the fingerprint of the node
that made it and a one-time secret. The joining node connects with TLS 1.3 and
continues only if the node it reached has that fingerprint. It then proves
that it holds the secret, and the proof is bound to that one connection. Both
nodes derive the **pair secret** (32 bytes) from that connection, and it never
crosses the network.

- A code is good for one pairing and for one hour. `-ttl` changes that, up to
  30 days. A code that has been used, has expired or was revoked can no
  longer be used.
- **Treat a code as a secret until it is used.** Whoever has it first can pair
  in your place. You would notice, because your own attempt is refused and the
  node that made the code lists a peer you do not know. If that happens, end
  that pairing.
- A code alone does not let anyone pose as the node that made it.
- After five wrong proofs for a code, the code is revoked.
- Enter a code through the web page or on the standard input of
  `tcpraid pair join`. A code given as a command-line argument shows in the
  process list and stays in the shell's history.

The node that made a code keeps the code's id and a hash of the secret, not
the code. That hash is what the proof is made with, so for as long as the code
is valid it is as good as the code. Someone who can read `state.json` on that
node could pair in the place of the node the code was meant for.

A pairing can be cut off, because it happens on two machines and cannot
complete on both at the same instant. If the node that made the code accepts
and its answer is lost on the way, that node lists the new peer and says so in
its log, while the joining node reports a failure. Pair them again with a new
code.

## What a peer may do

Pairing with a node means trusting it. Through a rule, a peer's connections
come out of your node and may go to **any address your node can reach**, on
your local network or beyond.

To narrow that, give the peer an allow list on the node where its connections
come out. The list holds networks with an optional port or range, separated by
commas. In the web page it is under Peers > Details > Advanced > "Destinations
the peer may reach". With the command:

```
tcpraid peers set nas-b -allow 192.0.2.10/32:873,192.0.2.10/32:50000-50010
```

`-allow ""` takes the list away again. The API has the same setting,
`PUT /api/v1/peers/NAME`, described in `reference-api.md`.

A connection to anything else is refused, and the application at the other
end sees a reset. The peer is told that its allow list entry was the reason,
and shows that in the status of its rule. When your node cannot connect to a
destination, the peer is told the error of the system, which names the
destination it asked for and nothing else.

The allow list does not apply to a path test (`tcpraid peers test`), because
the test data ends inside your node and reaches no address. A peer can make
your node receive such data whenever it likes. Your node bounds it without
relying on the peer: four test sessions at a time, each ended after 90
seconds or 1 GiB.

Ending a pairing (`tcpraid peers remove NAME`, or "End the pairing") resets
the sessions with that peer in both directions and refuses its connections
from then on. The other node is not told and still lists this one. Rules that
name the removed peer stay. A node that is paired later under the same name
takes them over, so remove or change them if that is not wanted.

## The data directory

| File | What it is worth |
|---|---|
| `identity.key` | the node's private key. With it, someone can be this node to its peers |
| `state.json` | holds the pair secrets and, while a code is valid, the hash of its secret. With a pair secret, someone can pass the opening of that pairing; in the `off` tier that is all it takes |
| `api.token` | full control of the node through the local API |
| `ui.password` | a salted hash (PBKDF2-HMAC-SHA256) of the web page's password, never the password |

The directory is created with mode 0700 and the files that hold a secret with
0600. The certificate, which is public, gets 0644. The node warns at start
when group or others can write to the directory or read the key or the token.
On Windows these modes do not say who has access. Access there follows the
permissions of the folder, and the node gives no such warning. **Back the
directory up like a private key, and do not share it between two nodes.**

## The web page and the local API

- **Plain HTTP.** The password, the token and everything else cross the
  network unencrypted between the browser or program and the node. By default
  the API listens on `127.0.0.1`. When you open it to other hosts
  (`-api 0.0.0.0:9850`, as in the container), do so on a network you trust, or
  put a reverse proxy with TLS in front. The node says so in its log.
- Anyone who is logged in, or has the token, can do everything. That includes
  pairing the node and, through a rule, reaching whatever a peer can reach.
  Treat the password like the token.
- A login lasts 12 hours or until "Log out". The cookie cannot be read by
  scripts and is not sent along with requests made by other sites. A request
  that changes something must also carry the header `X-Tcpraid-Request: 1`,
  which a page from another site cannot make a browser send.
- A browser keeps cookies per host name, not per port. So other web services
  at the same host name receive this cookie too, and two nodes on one host log
  each other out.
- After 5 wrong passwords in a row from one address, the login is closed to
  that address for a minute. Behind a reverse proxy all visitors share that
  count.
- The password is never stored. A generated one is printed once, in the log
  of the first start.

## Listeners and unauthenticated connections

Anyone who can reach a node's listener can open connections to it, so each
listener holds only a bounded number of connections that have not yet proved
who they are. The bound is 2048, or a quarter of the files the process may
open if that is less. A new connection is always taken. When the listener is
full, the connection that has waited longest is closed to make room. If one
source address holds more than an eighth of the places, the oldest connection
of the source that holds the most is closed instead. A connection that sends
nothing is closed after 3 seconds, and a handshake has 15 seconds.

This limits what holding connections open can do, but it does not make a
listener immune. Someone who brings about as many new connections as the
listener has places within the time a handshake takes can push a peer's
handshake out for as long as they keep it up. By estimate that is several
thousand connections a second, and it has not been measured. Connections that
are already established are never closed for this. A node keeps listening when
accepting fails, for example when it is out of file descriptors.

## What tcpraid does not do

- It does not encrypt between the application and its node, or between the
  far node and the destination.
- It does not protect a node whose data directory, token or password someone
  else has.
- It has had no independent security audit.
