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

A peer's rule may name its destination by a host name, which your node then
looks up for it. The allow list is applied to the addresses of the name, with
the port of the rule. Your node answers with the first address the list lets
through, and with no address at all when the list holds none of them. Without
an allow list the peer learns the first address of any name it asks for,
which is in line with what it may reach anyway. Your node does not pass on
the error of its resolver, so the peer does not learn which servers your
network asks. It looks up at most 32 names at a time and gives each lookup 5
seconds.

The allow list does not apply to a path test (`tcpraid peers test`), because
the test data ends inside your node and reaches no address. A peer can make
your node receive such data whenever it likes. Your node bounds it without
relying on the peer: four test sessions at a time, each ended after 90
seconds or 1 GiB.

Ending a pairing (`tcpraid peers remove NAME`, or "End the pairing") resets
the sessions with that peer in both directions and refuses its connections
from then on. The other node is not told and still lists this one. Rules that
name the removed peer stay. A node that is paired later under the same name
takes them over, so remove or change them if that is not wanted. The command
lists those rules, and `tcpraid peers remove NAME -with-rules` removes them
together with the peer.

## The data directory

| File | What it is worth |
|---|---|
| `identity.key` | the node's private key. With it, someone can be this node to its peers |
| `state.json` | holds the pair secrets and, while a code is valid, the hash of its secret. With a pair secret, someone can pass the opening of that pairing; in the `off` tier that is all it takes |
| `api.token` | full control of the node through the local API |
| `api.key` | the key the API and the web page are served with over TLS (`-api-tls`). With it, someone could stand in for the page and the API |
| `ui.password` | a salted hash (PBKDF2-HMAC-SHA256) of the web page's password, never the password |

The directory is created with mode 0700 and the files that hold a secret with
0600. The certificate, which is public, gets 0644. The node warns at start
when group or others can write to the directory or read the key or the token.
On Windows these modes do not say who has access. Access there follows the
permissions of the folder, and the node gives no such warning. **Back the
directory up like a private key, and do not share it between two nodes.**

## The web page and the local API

- **Plain HTTP unless you switch TLS on.** Without `-api-tls`, the password,
  the token and everything else cross the network unencrypted between the
  browser or program and the node. By default the API listens on `127.0.0.1`.
  When you open it to other hosts (`-api 0.0.0.0:9850`, as in the container),
  do so on a network you trust, put a reverse proxy with TLS in front, or
  start the node with `-api-tls`. The node says so in its log.
- **With `-api-tls` the page and the API are served over TLS**, on the same
  port, and a request without TLS is not served. At the first such start the
  node makes a key and a certificate for this and keeps them in the data
  directory (`api.key`, `api.crt`). No browser knows that certificate, so a
  browser asks before it shows the page. The log of the node has the SHA-256
  fingerprint of the certificate. Compare it with the fingerprint the browser
  shows for the certificate before you accept it. To use a certificate of
  your own instead, give it with `-api-cert` and `-api-key`.
- The commands on the host, `tcpraid status` and the others, follow by
  themselves. They read the certificate that the node recorded in the data
  directory (`api.tls`) and talk only to an API that holds its key. To an API
  that presents another certificate they send nothing, and the token does not
  leave the host.
- Anyone who is logged in, or has the token, can do everything. That includes
  pairing the node and, through a rule, reaching whatever a peer can reach.
  Treat the password like the token.
- A login lasts 12 hours or until "Log out". The cookie cannot be read by
  scripts and is not sent along with requests made by other sites. A request
  that changes something must also carry the header `X-Tcpraid-Request: 1`,
  which a page from another site cannot make a browser send.
- A browser keeps cookies per host name, not per port. So other web services
  at the same host name receive this cookie too. Since version 0.8.0 the
  cookie is named after the port the browser used to reach the node, so two
  nodes at two ports of one host keep their logins apart. A login made before
  an upgrade to 0.8.0 ends with the upgrade, and you log in once more. Over
  TLS the cookie is marked for TLS only.
- After 5 wrong passwords in a row from one address, the login is closed to
  that address for a minute. Behind a reverse proxy all visitors share that
  count. The login itself needs the header `X-Tcpraid-Request: 1`, and a
  login without it is refused before the password is looked at, so a page of
  another site cannot use up the five tries of its visitor's address.
- The node checks one password at a time for one address and four at a time
  in all, and a login beyond that is told to try again. A wrong password from
  one address does not hold up the login of another. These are limits and no
  more: someone who sends logins from many addresses can still keep the four
  places busy while they do.
- The password is never stored. A generated one is printed once, in the log
  of the first start.
- A node that needs no web page can be started with `-no-ui`. Its pages and
  the login then answer `404`, no password exists, and only the token gives
  access to the API.

## The figures, the hook and the exported file

Three things carry information about the node out of it. Each is off until
you switch it on or run it.

| What | Who gets it | What it holds |
|---|---|---|
| the figures of `-metrics` | whoever can reach that address. No token is asked for | numbers, and the names of the peers and the rules. No address, no destination, no key or token, no text of a failure |
| the events of `-hook` | the program you name, which runs as the node's user | names of peers and rules, destinations, byte counts and reasons. No key, no token, no pairing code |
| the file of `tcpraid config export` | whoever you give the file to | names, fingerprints, addresses, allow lists, rules and settings. No private key, no pair secret, no token, no password, no pairing code, and no user or password of a proxy |

The address of `-metrics` serves the figures and nothing else, takes no
request that changes anything, and answers four requests at a time. It is
not the API: every path of the API still needs the token. Keep the address
on loopback or on a network where the names of your peers and rules may be
read.

The program of `-hook` is run directly, not through a shell, and gets no
arguments. A reason can hold text that the other node sent, and that text
reaches the program only as the value of a variable and inside the JSON on
its input. What the program then does with it is the program's business: a
script that passes `$TCPRAID_REASON` on to another command has to quote
it. The program inherits the environment of the node, without the
node's own variables that begin with `TCPRAID_`.

The exported file describes how the node is reached and what it may reach,
so protect it like other configuration. It is not enough to be
the node, or to pair with its peers.

A program that uses the Go package (`go-package.md`) reads the token from
the data directory, like the commands. It can do what the API offers.

## Who can send connections through a node

A node asks nobody who they are at the places where applications hand it
their connections. Who can use each of them is decided by who can reach it:

| Way in | Who can use it |
|---|---|
| a fixed rule | whoever can connect to the rule's listen address |
| a transparent rule | whoever's connections the host redirects to the node |
| a SOCKS5 rule | whoever can connect to the rule's address. It listens on the loopback address unless you give another |
| `tcpraid connect`, `POST /api/v1/connect` | whoever holds the token of the local API, or is logged in to the web page |

A fixed rule and a transparent rule reach the destinations that the rule
names. A SOCKS5 rule and `tcpraid connect` reach any destination that is
asked for. So a SOCKS5 rule on an address that other hosts can reach gives
every one of those hosts, through the peer, all that the peer's allow list
lets your node reach. Keep such a rule on loopback, or on a network where
that is what you want, and give the peer an allow list on its side.

Every one of these ways ends at the same check: the peer holds the
destination against the allow list it keeps for your node, also when the
destination was given as a name.

A SOCKS5 rule bounds the clients that have not finished their request: 64 at
a time for a rule, each for 10 seconds. That limits what idle connections
can hold, and does not make the listener immune to someone who keeps
opening them.

With `-redirect` the node runs the program `nft` and needs the capability
`CAP_NET_ADMIN`. That capability lets a process administer the packet filter
of the host, and its interfaces and routing tables as well. The node uses it
for one table (`reference-files.md`). Without `-redirect` the node needs no
such capability.

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
