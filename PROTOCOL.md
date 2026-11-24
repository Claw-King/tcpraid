# tcpraid wire protocol

Version 1 (`wireVer = 1`). All integers are big-endian unless stated otherwise.
`uvarint` is the unsigned LEB128 encoding used by Go's `encoding/binary`.

## Roles

A **session** carries one TCP stream in each direction between a client and a
server. The client opens all sub-connections (TCP), and the server listens. Each
sub-connection is either a **data** sub (carries chunks and snapshots), a
**repair** sub (a data sub that carries re-sent chunks only, and new chunks only
while no ordinary data sub of the session is up) or a **control** sub (snapshots
only). Optionally, snapshots also travel as UDP
datagrams. Both ends send data and snapshots. The protocol is symmetric after
the handshake.

## Keys

With a pre-shared key `psk` (16 bytes or more):

```
hdrKey     = HMAC-SHA256(psk, "tcpraid v1 header")
snapKeyC2S = HMAC-SHA256(psk, "tcpraid v1 snapshot c2s")   (snapshots sent by the client)
snapKeyS2C = HMAC-SHA256(psk, "tcpraid v1 snapshot s2c")   (snapshots sent by the server)
MAC(k, m)  = first 16 bytes of HMAC-SHA256(k, m)
```

Without a PSK all MAC fields are zero and are not checked. UDP needs a PSK.

## Sub-connection header (client -> server, 69 bytes)

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | `"TCPR"` |
| 4 | 1 | version (1) |
| 5 | 1 | flags: bit 0 new session, bit 1 control sub, bit 2 repair sub |
| 6 | 16 | session id (random, chosen by the client) |
| 22 | 16 | destination IP (IPv4-mapped for IPv4) |
| 38 | 2 | destination port (0 with the address 0.0.0.0: the session of a path test, see "Between paired nodes") |
| 40 | 1 | path index (client's numbering) |
| 41 | 2 | sub index (unique within the session) |
| 43 | 2 | client window in MB |
| 45 | 8 | Unix time (seconds) |
| 53 | 16 | `MAC(hdrKey, bytes 0..52)` |

With a PSK the server rejects a bad MAC or a time more than 10 minutes off.
Only a sub with the *new session* flag may create a session. Any other sub for
an unknown session is answered "gone".

## Reply (server -> client, 8 bytes)

`"TCPR"` | status (1) | reserved (1) | server budget in MB (2)

Status: 0 ok, 1 gone (the session ended cleanly), 2 gone (unknown or ended
badly), 3 refused (session limit), 4 bad header or MAC. After a non-zero status
the server half-closes and lingers briefly so the reply survives relays.

## Frames (both directions, after the reply)

| Type | Layout | Notes |
|---|---|---|
| 1 DATA | type, seq (8), send time (8), len (4), payload | data subs only; payload 1..1 MiB |
| 2 SNAP | type, len (2), snapshot, MAC (16) | `MAC(snapKey of the sender's direction, snapshot)` |
| 3 ABORT | type | the sender's application connection failed or it is exiting: reset yours (also sent as a UDP snapshot with the aborted flag) |
| 4 DIALFAIL | type | server: the destination is unreachable or not allowed |

`seq` numbers chunks from 0 per direction. `send time` is the sender's
monotonic clock in nanoseconds. Only the sender interprets echoed send times.

## Snapshot

A snapshot is one end's complete state. A newer one (higher `seqNo`) replaces
every older one, so snapshots may be lost, duplicated or reordered freely.

```
seqNo      uvarint   strictly increasing per sender
sentNs     8 bytes   sender monotonic clock (ns)
cum        uvarint   next chunk expected: everything below was delivered in order
nRanges    uvarint   <= 64
  per range: lo - prev (uvarint), hi - lo (uvarint)     prev starts at cum, then the last hi
nGaps      uvarint   <= 32
  per gap:   lo - prev, hi - lo, ageMs (uvarints)       missing [lo, hi): later chunks have
                                                        waited ageMs for it (receiver's clock,
                                                        excluding time blocked on its own output)
nSubs      uvarint
  per sub:   index (uvarint), bytes received on it (uvarint), echo (8 bytes: send time of the
             latest DATA received on it)
budget     uvarint   the receiver's reorder budget (bytes)
fin+1      uvarint   0 = the sender's stream has not ended; else its end-of-stream chunk + 1
flags      1 byte    bit 0: the peer's stream was delivered up to its end
                     bit 1: all done (both directions complete)
                     bit 2: aborted -- the sender is resetting this session (its application
                            connection failed or it is exiting); reset yours as for ABORT
```

Ranges and gaps are the ones nearest `cum`. A snapshot that cannot hold them
all is truncated, and the reader must not infer anything beyond the last
range. Rules for the receiver of data (the sender of the snapshot):

1. A chunk reported received is never discarded before it is delivered.
2. A chunk below the highest one received is always accepted. A new highest one
   is accepted only within the reorder budget.

## UDP datagram

```
tag (8) | nonce (12) | AES-256-GCM(key, nonce, snapshot, aad = tag)
tag   = first 8 bytes of HMAC-SHA256(hdrKey, "udp tag" || session id)
key   = HMAC-SHA256(snapKey of the sender's direction, session id)
nonce = 4 zero bytes || 8-byte counter (per session and direction)
```

One end listens on a configured address, and the other sends to it. A datagram whose
counter is not above the highest one accepted for that session and direction is
dropped (replay guard). The listener answers each session at the source address
of the last accepted datagram. Datagrams that fail authentication are dropped
(and rate limited).

## Sending side behaviour (informative)

- Each data sub holds at most its delivery rate x (base feedback loop + target)
  of unreported data. The base loop is the lowest send-to-echo time seen on the
  sub.
- Chunks reported received are released at once (window and memory).
- A chunk in a gap is re-sent alone, to the sub with the shortest queue (not the
  one that carried it, and on another path when one is ready), once its age
  exceeds `hol` + target for each attempt.
- New data is read only within the send window and the peer's reorder budget.

## Nodes: identity, listeners, pairing

**Identity.** A node has an Ed25519 key pair and a self-signed X.509
certificate for it. Its **fingerprint** is the SHA-256 of the certificate's
SubjectPublicKeyInfo, written as 64 hex digits.

**Listeners.** A node's TLS listener takes TLS 1.3 only and tells connections
apart by the protocol name (ALPN) the client asks for:

| Protocol name | Connection |
|---|---|
| `tcpraid-pair/1` | a node that redeems a pairing code (below) |
| `tcpraid/1` | a paired node. Both ends show their identity certificates; each checks the other's fingerprint against the one it stored at the pairing |
| `tcpraid-own/1` | a paired node that expects the certificate the operator supplied. The listening end shows that certificate; the dialing end checks it by the fingerprint of its key, or by authority and host name, and shows its identity certificate |

Anything else is closed. A node may also have a plain TCP listener, for paths
that something else encrypts. On all of them, what follows is described in
"Between paired nodes".

**Pairing code.** One line of text:

```
tcpraid-pair-1-<base32>
```

`1` is the version of the format. The base32 part (RFC 4648 alphabet in lower
case, no padding) encodes a payload followed by the first 4 bytes of its
SHA-256:

```
id (4) | one-time secret (16) | fingerprint (32) | expiry (8) | n (1) | n x ( len (1) | address )
```

The `id` names the code and is not secret. The `fingerprint` is that of the
node that made the code. The `expiry` is Unix time in seconds, big endian. The
addresses (1 to 8) are `host:port`, where the joining node connects. The node
that made the code keeps the id, the expiry and the SHA-256 of the secret,
not the secret.

**Pairing exchange.** The joining node connects to one of the addresses with
TLS 1.3 and the protocol name `tcpraid-pair/1`, shows its identity certificate,
and continues only if the certificate it is shown has the fingerprint in the
code. It then sends

```
id (4) | proof (32) | n (1) | name (n)

proof = HMAC-SHA256(SHA-256(one-time secret), exporter("tcpraid pairing proof v1", 32 bytes))
```

where `exporter` is the keying material exported from this TLS connection
(RFC 8446, section 7.5) under that label with an empty context, and `name` is
the joining node's name. The proof shows that the joining node holds the code
and is good for this connection only. The node that made the code answers

```
status (1) | n (1) | name (n)
```

with status 0 and its own name when it accepted, or another status and
nothing more when it did not. It does not say why. A code is accepted once,
before its expiry. After five wrong proofs it is revoked. The whole exchange
has 15 seconds.

Both ends then take the **pair secret**: 32 bytes exported from the same
connection under the label `tcpraid pair secret v1`. They share it without
ever sending it. Each stores the other's name and fingerprint with it.

## Between paired nodes

Two paired nodes (`tcpraid run`) use the protocol above unchanged, with two
additions. The node that carries a connection is the client, the node at the
other end the server.

**Keys.** The nodes share a 32-byte pair secret from their pairing. The `psk`
of the section "Keys" is

```
psk        = HMAC-SHA256(pair secret, "tcpraid transport key v1")
openingKey = HMAC-SHA256(pair secret, "tcpraid link opening v1")
```

**Opening.** Every connection between two nodes, inside TLS or on plain TCP,
starts with 88 bytes from the dialing node:

```
"TCPL" | version (1) = 1 | type (1) | fingerprint (32) | Unix time (8) |
nonce (16) | request (8) | path (1) | role (1) | MAC (16)
```

`fingerprint` is the SHA-256 of the dialing node's public key
(SubjectPublicKeyInfo). `MAC` is `MAC(openingKey, the 72 bytes before it)`. The
listening node finds the peer with that fingerprint, checks the MAC, refuses a
time more than 10 minutes from its own clock and, under TLS, a fingerprint that
is not the one TLS authenticated. A connection that fails any of this is closed
without an answer. `request`, `path` and `role` are set in type 2 and zero in
the others.

| Type | Then |
|---|---|
| 1 | a sub-connection: the dialing node sends the 69-byte header, as above |
| 2 | a sub-connection that the listening node asked for (below): the **listening** node sends the 69-byte header |
| 3 | the control connection (below) |
| 4 | a check: the listening node answers `status (1) = 0` and `MAC(openingKey, "tcpraid link check v1" \| 0x00 \| nonce)`, 17 bytes in all, and the connection ends |

A session belongs to the pair of nodes that created it. A sub-connection from
another peer that names its id is answered like one for a session the server
does not hold (status 2).

**Control connection.** A node keeps one control connection (type 3) to every
peer it has a path to. After the opening, each end sends messages:

```
length (2, big endian) | JSON | MAC (16)

controlKey = HMAC-SHA256(pair secret, "tcpraid control v1")
MAC        = MAC(controlKey, length | JSON)
```

`length` counts the JSON, at most 16384 bytes. A longer message, or a wrong
MAC, ends the connection. The JSON is an object with a `type`. A type or a
field that the receiving end does not know is ignored.

| `type` | Sent | Fields | Meaning |
|---|---|---|---|
| `hello` | by both ends, first | `name`, `version`, `protocols` (list of numbers; this document is 1), `instance` (text; optional), `features` (list of text; optional) | who is there |
| `ping` | by both ends, every 10 s | `seq` | the connection is alive |
| `pong` | in answer to a `ping` | `seq` | |
| `open` | by the end that carries a session | `request` (number, not zero), `session` (the id in hex), `test_path` and `test_subs` (numbers; optional) | open the sub-connections of this session from your end |
| `open_fail` | in answer to an `open` | `request`, `reason` (text) | it cannot be done |
| `session_fail` | by the exit end of a session | `session` (the id in hex), `code` (`allow` or `dial`), `reason` (text) | the session could not be connected to its destination, and why |
| `paths` | by an end that runs a path test | `request` | name the paths you hold to me |
| `paths_reply` | in answer to a `paths` | `request`, `paths` (list of `name`, `subs`, `data`) | the paths, in their order, without addresses |
| `test_end` | by an end that runs a path test | `session` (the id in hex) | I have ended this session of the test |

The last four types, and the fields `features`, `test_path` and `test_subs`,
were added in version 0.3.0. Older versions ignore them.

An end that receives nothing for 30 seconds takes the connection for dead. The
end that dialed dials again after 1, 2, 4, then every 8 seconds.

**A peer that was restarted.** `instance` is a number that the sending process
picked at random when it started (16 hexadecimal digits), so it is another one
after the node was restarted or killed and started again. A new process holds
none of the sessions of the old one. An end that receives a `hello` whose
`instance` differs from the one it last received from that peer resets the
sessions it still holds with the peer, if they began before the last message
it received on a control connection of the old process. A session that began
after that message is left to the ordinary rules. Nothing is reset on the
first `instance` an end receives from a peer, on the same one again, on the
one before the current one (a late message of the old process), or on a
`hello` without `instance` (versions before 0.1.2 send none and ignore it).

**Why a session could not be connected.** The exit end of a session tells the
client end in the transport that it could not connect the session to its
destination (frame DIALFAIL), without a reason. Since version 0.3.0 it also
sends `session_fail` on a control connection first, if one is up. `code` is
`allow` when the destination is not in the allow list that the exit end keeps
for the other node, and `dial` when connecting failed, in which case `reason`
is the error of the system. The text is for a person to read, and the
receiving end shows at most 200 bytes of it with everything but printable
ASCII replaced. An end that receives `session_fail` for a session it carries
with that peer ends the session as it does on DIALFAIL. This matters because
the frame can be lost when the exit end closes its connections right after
it. A `session_fail` that names a session the receiving end does not carry
with that peer changes nothing.

**Path test.** Since version 0.3.0 a node can measure a path by sending data
that the other node discards. A node that takes part says so with the text
`path-test` in the `features` of its `hello`, and no test is started with a
peer that does not. The session of a test is an ordinary session whose
destination in the 69-byte header is the IPv4 address 0.0.0.0 and port 0.
The exit end connects such a session to nothing. It reads the data and
throws it away, and it does not consult the allow list, because the session
reaches nothing outside the node. The exit end bounds every such session by
itself. It takes 4 of them at a time, and ends one after 90 seconds or after
1 GiB of data. A node that does not know this destination treats it like any
other and fails to connect it. The node that runs the test ends each of its
sessions with ABORT and also sends `test_end` on a control connection,
because the frame is often lost behind test data that is still on its way.
The exit end ends a session of a test that the peer names in a `test_end`.
A session of a test that the other end has answered ends when nothing
arrives for 30 seconds, without waiting for the grace time.

A test measures one path at a time, first with one data sub-connection and
then with all the path has. The dialing node knows its own paths. A node
that holds no path asks the peer with `paths`. The peer
answers with `paths_reply`, which lists for each path it holds to the asking
node the `name`, the number of data sub-connections `subs`, and whether the
path carries data (`data`). The asking node then sends `open` with
`test_path`, the position of the path in that list counted from 1, and
`test_subs`, the number of data sub-connections to keep on it (0 means as
many as the path has). The peer opens the control and repair sub-connections
of all its paths as usual, and no data sub-connection on any other path. An
`open` without `test_path`, or with 0, is the ordinary request.

**A session whose sub-connections the exit end opens.** A node that has to
carry a connection to a peer it holds no path to creates the session, picks a
`request` number and sends `open` on a control connection with that peer. The
request names no address. The peer dials back over its own paths, as many
connections as its paths ask for, each with an opening of type 2 that carries
the `request`, the index of the path among the dialing node's paths, and the
role (0 data, 1 repair, 2 control). On each, the node that asked sends the
ordinary 69-byte header of the session (with the `new` bit until the peer has
answered once) and the peer answers with the ordinary 8-byte reply. From there
on the sub-connection is like any other. The roles of the two ends in the
transport are unchanged. The node that asked is the client of the session, and
the node that dialed is the server.

The node that dialed keeps the number of sub-connections up while the session
lives, with a client's limits on redials, each session counted on its own. The node that asked
never dials. A connection of type 2 whose `request` the listening node did not
make to that peer, or whose session has ended, is closed. If no sub-connection
arrives within 15 seconds of the `open`, or an `open_fail` arrives, the node
that asked resets the application connection.
