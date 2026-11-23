# Security

What tcpraid protects, what it does not, and what each secret is worth. This
is about nodes (`tcpraid run`); manual mode has its own section in
`manual-mode.md`.

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

Use `off` only on a path that something else encrypts from node to node: an
SSH tunnel, a VPN, an encrypted proxy chain. In that tier the opening is the
only proof of who dials, and an opening that someone recorded on the path
could be sent again within ten minutes.

In every tier, a connection between two nodes starts with a short opening --
the dialing node's fingerprint, the time and a random number under a MAC
(HMAC-SHA256) with a key derived from the pair secret. The listening node
closes a connection whose opening it cannot verify, or whose time is more than
ten minutes from its own clock, without an answer. The transport then
authenticates its own headers and feedback with another key derived from the
pair secret. No two pairs of nodes share these keys.

Everything is from the standard library of Go: TLS 1.3, X.509, Ed25519,
HMAC-SHA256, AES-256-GCM for UDP feedback. There is no cryptography of
tcpraid's own design.

## Pairing

A pairing code holds the address to connect to, the fingerprint of the node
that made it and a one-time secret. The joining node connects with TLS 1.3 and
continues only if the node it reached has that fingerprint; it then proves,
bound to that one connection, that it holds the secret. Both nodes derive the
**pair secret** (32 bytes) from that connection; it never crosses the network.

- A code is good for one pairing and for one hour (`-ttl` changes that, up to
  30 days). Used, expired or revoked, it is worth nothing.
- **Treat a code as a secret until it is used.** Whoever has it first can pair
  in your place. You would notice: your own attempt is refused, and the node
  that made the code lists a peer you do not know. End that pairing.
- A code alone does not let anyone pose as the node that made it.
- After five wrong proofs for a code, the code is revoked.
- Enter a code through the web page or on the standard input of
  `tcpraid pair join`. Given as a command-line argument it shows in the
  process list and stays in the shell's history.

The node that made a code keeps the code's id and a hash of the secret, not
the code. That hash is what the proof is made with, so for as long as the code
is valid it is as good as the code: someone who can read `state.json` on that
node could pair in the place of the node the code was meant for.

**A pairing can be cut off.** It happens on two machines and cannot complete
on both at the same instant. If the node that made the code accepts and its
answer is lost on the way, that node lists the new peer (its log says so) and
the joining node reports a failure. Pair them again with a new code.

## What a peer may do

Pairing means trust. Through a rule, a peer's connections come out of your
node and may go to **any address your node can reach**, on your local network
or beyond.

To narrow that, give the peer an allow list on the node where its connections
come out: networks with an optional port or range, separated by commas. In the
web page: Peers > Details > Advanced > "Destinations the peer may reach". With
the API:

```
curl -X PUT -H "Authorization: Bearer $(cat /var/lib/tcpraid/api.token)" \
     -d '{"allow": "192.0.2.10/32:873,192.0.2.10/32:50000-50010"}' \
     http://127.0.0.1:9850/api/v1/peers/nas-b
```

A connection to anything else is refused, and the application at the other
end sees a reset.

Ending a pairing (`tcpraid peers remove NAME`, or "End the pairing") resets
the sessions with that peer in both directions and refuses its connections
from then on. The other node is not told and still lists this one. Rules that
name the removed peer stay; a node that is paired later under the same name
takes them over, so remove or change them if that is not wanted.

## The data directory

| File | What it is worth |
|---|---|
| `identity.key` | the node's private key. With it, someone can be this node to its peers |
| `state.json` | holds the pair secrets and, while a code is valid, the hash of its secret. With a pair secret, someone can pass the opening of that pairing; in the `off` tier that is all it takes |
| `api.token` | full control of the node through the local API |
| `ui.password` | a salted hash (PBKDF2-HMAC-SHA256) of the web page's password, never the password |

The directory is created with mode 0700 and the files that hold a secret with
0600 (the certificate, which is public, with 0644). The node
warns at start when group or others can write to the directory or read the
key or the token. **Back it up like a private key, and do not share it between
two nodes.**

## The web page and the local API

- **Plain HTTP.** The password, the token and everything else cross the
  network unencrypted between the browser or program and the node. By default
  the API listens on `127.0.0.1`. When you open it to other hosts (`-api
  0.0.0.0:9850`, as in the container), do so on a network you trust, or put a
  reverse proxy with TLS in front. The node says so in its log.
- Whoever is logged in, or has the token, can do everything: pair the node,
  and through a rule reach whatever a peer can reach. Treat the password like
  the token.
- A login lasts 12 hours or until "Log out". The cookie cannot be read by
  scripts and is not sent along with requests made by other sites; a request
  that changes something must also carry the header `X-Tcpraid-Request: 1`,
  which a page from another site cannot make a browser send.
- A browser keeps cookies per host name, not per port: other web services at
  the same host name receive this cookie too, and two nodes on one host log
  each other out.
- After 5 wrong passwords in a row from one address, the login is closed to
  that address for a minute. Behind a reverse proxy all visitors share that
  count.
- The password is never stored. A generated one is printed once, in the log
  of the first start.

## Listeners and unauthenticated connections

Anyone who can reach a node's listener can open connections to it, so each
listener holds only a bounded number of connections that have not yet proved
who they are: 2048, or a quarter of the files the process may open if that is
less. A new connection is always taken; when the listener is full, the one
that has waited longest is closed to make room, or, if one source address
holds more than an eighth of the places, the oldest connection of the source
that holds the most. A connection that sends nothing is closed after 3
seconds, and a handshake has 15 seconds.

This limits what holding connections open can do. It does not make a listener
immune: someone who brings about as many new connections as the listener has
places within the time a handshake takes -- several thousand a second by
estimate, which has not been measured -- can push a peer's handshake out for
as long as they keep it up. Connections that are already established are
never closed for this. A node keeps listening when accepting fails, for
example when it is out of file descriptors.

## What tcpraid does not do

- It does not encrypt between the application and its node, or between the
  far node and the destination.
- It does not protect a node whose data directory, token or password someone
  else has.
- It has had no independent security audit.
