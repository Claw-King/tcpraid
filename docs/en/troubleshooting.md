# Troubleshooting

Where to look first: the **rule's status** (Rules page, or `tcpraid rules`),
the **peer's state** (Peers page, or `tcpraid peers`), and the **node's log**
(`docker logs tcpraid`, `journalctl -u tcpraid-node`).

## The application cannot connect, or is cut off at once

A rule that cannot carry a connection resets it and keeps the reason:

```
nas-a$ tcpraid rules
NAME  TAKES         PEER   DESTINATION         SESSIONS  STATE
back  0.0.0.0:9811  nas-b  198.51.100.20:7100  0         listening; last connection: this node has no path to peer "nas-b", and the peer is not connected to this node (it would open the connections): start the peer, or give this node a path to it (tcpraid peers path set)

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
taken  0.0.0.0:9700  nas-a  192.0.2.10:873  0         FAILED: listening on 0.0.0.0:9700 failed: listen tcp 0.0.0.0:9700: bind: address already in use
```

| What you see | What it means |
|---|---|
| `STATE` is `FAILED` with a reason | the rule could not listen: the port is taken, or not allowed. The other rules run. It is tried again when the rules change |
| "this node has no path to peer ... and the peer is not connected" | the peer is offline, and this node cannot dial it. Start the peer, check its path to this node |
| connection refused before any of this | nothing listens on that port. In a container: is the rule's port published, and does the rule listen on `0.0.0.0`? |
| the connection is accepted and reset at once | the far node could not reach the destination, or the peer's allow list does not include it. The far node's log says which: `dial ADDRESS: ... connection refused`, or `dial ADDRESS: not in the allow list of the peer`; this node's log says `server could not reach ADDRESS` |
| reset after 15 seconds, rule on the node without a path | the peer was asked to connect and nothing arrived: the peer can no longer reach this node's listener |
| reset after 30 seconds, and the rule's status shows no reason | none of the sub-connections came up. One cause: the pairing was ended on the other node. `tcpraid peers check NAME` then says "the other node does not accept this node: it is not paired with it, or no longer", and the other node's log has lines "refused a connection from ...: not a peer of this node". Pair again |

A destination is an address **as the far node sees it**. With the far node in
a container, `127.0.0.1` is the container, not the NAS.

## The peer is offline

`online` means a control connection between the two nodes is up. The node that
holds a path opens it and tries again every 8 seconds.

```
nas-b$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok
```

`peers check` opens a connection on each path and asks the peer to prove that
it holds the pairing. If it fails, the message says where:

- `FAILED: dial tcp ...: connect: connection refused` (or a timeout): the
  address, a port forward on the way, or the peer's `-listen` port;
- `FAILED: tls: first record does not look like a TLS handshake`: the address
  leads to something that is not a node's TLS listener, for example the
  node's port 9850 instead of 9851;
- `FAILED: ... connection reset by peer (the other node closed the connection
  during the handshake; it may be busy: try again)`, every time: besides a
  busy node, this is what a path of the `tls` tier gets from a node's plain
  listener (`-listen-plain`). Check that the port and the path's tier belong
  together;
- "the node at this address is not the peer: its key has another
  fingerprint": another node answers there, or the peer has a new identity
  (its data directory was lost);
- "the other node does not accept this node: it is not paired with it, or no
  longer": the pairing was ended on the other node;
- "the other end closed the connection without answering": on a path of the
  tier `off`, the other node is not paired with this one; in any tier, the two
  hold different pair secrets after a pairing that was cut off, or their
  clocks are more than ten minutes apart (the other node's log then says so).
  Pair them again, or set the clock.

A node without a path to its peer cannot check (`this node has no path to
...: it can only be reached by it`); check from the other side.

## Pairing fails

- "no address for the other node to connect to": the node does not know its
  outside address. Give one when making the code (`-addr HOST:PORT`, or the
  field in the web page), or start the node with `-advertise`.
- "the other node did not accept the pairing code": the code was used, has
  expired (one hour by default), was revoked or was not copied whole.
- "the pairing code is damaged": copy it again, complete and unchanged.
- "the node at this address is not the one that made the pairing code": the
  address leads to another node.
- A join that cannot finish is given up after 60 seconds.

## It works but it is not faster

- Measure the path first without tcpraid: `iperf3` with one stream and with
  eight. If eight together are not clearly faster than one, there is nothing
  for tcpraid to gain.
- Look at the session (Sessions page, `tcpraid sessions`, or
  `GET /api/v1/sessions`): `subs` should be the number of connections of the
  paths (8 by default), and `resent_bytes` a small share of `tx_bytes` (the
  column `SENT AGAIN` of the command). Many bytes sent again point to
  a path that loses or stalls; a second path with the roles `repair` and
  `control`, or UDP feedback, is what helps there (`concepts.md`).
- The speed of the stretch between the application and its node, and between
  the far node and the destination, counts as well: tcpraid cannot be faster
  than the slower of them, or than the application itself.
- When a session ends, both nodes log two lines about it: bytes, chunks sent
  again, sub-connections replaced, time stalled. A node started with
  `-timeline` logs the same kind of figures while the session runs. The next
  section explains them, and how they tell whether `-window` or `-hol`
  (`reference-files.md`) is worth changing.

## Reading the session lines in the log

When a session ends, each node logs two lines about it. A node started with
`-timeline 2s` also logs one line per session every two seconds while it
runs (the first one about half a second after the session begins). The figures are those of the node that logs them: the sending figures
matter at the end the data comes from, the receiving figures at the other.

### The timeline line

```
[ddc465] tl t=3.0 tx=34.2 re=0.0 ack=37.1 rx=0.0 dlv=0.0 unacked=0 unsent=0 pend=0 infl=0 sacked=63 fbage=0.03 fb=144/9 fbc=0 fbu=0 cap=0/8 holage=0.00 pumpwait=0.28 starve=0.94 loop=243/1521
```

Rates are MB/s over the time since the previous line; sizes are MB at the
moment of the line.

| Field | Meaning |
|---|---|
| `t` | seconds since the session began |
| `tx` | stream data sent for the first time |
| `re` | stream data sent again |
| `ack` | data the peer reported received, which frees it here |
| `rx` | data received from the peer, copies of what it already had included |
| `dlv` | data written, in order, to the application on this side |
| `unacked` | data read from the application that the peer has not reported received. It includes `unsent` |
| `unsent` | the part of `unacked` that has not been sent yet: it waits for room on a sub-connection |
| `pend` | data received and not yet written to the application: it waits for an earlier chunk, or for the application to take it |
| `infl` | data in flight: sent on the sub-connections that carry data and not yet reported received |
| `sacked` | what the peer holds as its `pend`: received, not yet delivered |
| `fbage` | seconds since the last report from the peer arrived (`-1`: none yet) |
| `fb=IN/OUT` | reports received and sent since the previous line (of those sent, the copies over UDP are not counted) |
| `fbc`, `fbu` | of the reports received, those that came on control connections and over UDP |
| `cap=A/N` | of the `N` live sub-connections that carry data (roles `data` and `repair`), `A` hold as much in flight as they are allowed |
| `holage` | receiving: seconds the oldest data in `pend` has waited for a chunk that is missing before it |
| `pumpwait` | share of the time since the previous line that this node did not read from the application because `-window`, the peer's window or `-budget` was full. A wait is counted in the line during which it ends: one wait that lasts over several lines shows as 0 in them and then as a value above 1 |
| `starve` | share of the time an average data sub-connection had room and nothing to send. Sub-connections of a `repair` path count and are idle most of the time |
| `loop=P50/P95` | milliseconds from sending data on a sub-connection to the report that it arrived, median and 95th percentile. It includes the time the data queued on the way. A sub-connection that carries nothing new keeps reporting its last data, so the figure grows while sub-connections are idle (those of a `repair` path among them) |

What limits a transfer shows at the sending node:

- `pumpwait` near 0 while data moves (`tx` above zero): the node waits for
  the application; the path is not the limit. (`pumpwait` 0 with `tx` 0 over
  several lines is one long wait that has not ended yet: read it as one of
  the cases below.)
- `pumpwait` near 1 with `unsent` large and every sub-connection of the data
  paths at its cap: the application delivers faster than the path takes. The
  window is full of data that has not been sent yet; a larger `-window` holds
  more of it and sends no faster.
- `pumpwait` near 1 with `unsent` near 0: the data is out, and the window is
  full of what the peer has not reported or cannot deliver yet (`sacked`
  large). Look at the receiving node. If its `holage` is above zero, data
  waits there for missing chunks: a larger `-window` at both nodes gives it
  more room, and a shorter `-hol` at the sending node sends a missing chunk
  again sooner, at the price of some chunks sent twice. If its `holage` stays
  at zero while its `pend` is large, the application there is slow to take
  the data (`stall` in its end line says for how long), and no setting of
  tcpraid changes that. If its `holage` is zero and its `pend` is small as
  well, nothing waits there: the window is smaller than what the path holds
  in flight, and a larger `-window` at both nodes may let more through.

### The two lines at the end

```
[ddc465] session end: done (6.2s tx 200MB rx 0MB chunks 800 resends 0 kicks 0 subs 8 stall 0.0s)
[ddc465] session diag: pump win 0.7s | resent | subs died lost=0 silent=0 write=0 slow=0 | fb gap max 0.0s >1s 0 | gap repairs 0 (retries 0) ms p50/p95/max: peer wait 0/0/0, queued 0/0/0, to release 0/0/0 | snapshots truncated 0
```

`session end` (or `session aborted`) gives the reason, then:

| Field | Meaning |
|---|---|
| first number | how long the session lasted |
| `tx`, `rx` | stream data sent (first time only) and received |
| `chunks` | chunks sent for the first time |
| `resends` | chunks queued to be sent again |
| `kicks` | sub-connections replaced for being much slower than the others of their path |
| `subs` | sub-connections that came up over the life of the session |
| `stall` | time spent in writes to the application that each took over 50 ms: the application, or the stretch to it, was slow to take the data |

`session diag`:

| Field | Meaning |
|---|---|
| `pump win` | total time this node did not read from the application because a window or the budget was full (the sum behind `pumpwait`) |
| `resent gap=N stall=N dead=N` | why chunks were queued again: the peer reported later data waiting for it (`gap`); it was not reported received long after it was due (`stall`); its sub-connection ended (`dead`). Causes with a count of zero are left out |
| `subs died lost / silent / write / slow` | how sub-connections ended: the connection failed or was closed (`lost`, which includes the ones the other node closes when the session finishes); no progress for 20 seconds (`silent`); a write that did not finish in time (`write`); replaced for being slow (`slow`) |
| `fb gap max`, `>1s N` | the longest time between two reports from the peer while data was unacknowledged, and how often it was over a second |
| `gap repairs N (retries R)` | chunks that were sent again because the peer reported them missing and that it then reported received (the count stops at 4096; `resent gap=` has them all), and how many sends for a missing chunk were not the first for it |
| `peer wait` | for those chunks: how long later data had waited at the peer when this node decided to send the chunk again. **It is not a property of the path.** The decision is taken once the wait is over `-hol` plus the allowed queue (300 ms; 1 s for a while after reports stalled), so with the defaults it is never below 1.3 s and usually just above |
| `queued` | from that decision until the chunk was handed to a sub-connection |
| `to release` | from that decision until the peer reported the chunk received |
| `snapshots truncated` | reports this node built that could not list every range it holds out of order |
| `udp in / out / gap max` | with UDP feedback: datagrams accepted and sent for the session, and the longest time between two accepted ones |

The three times are in milliseconds: median, 95th percentile, maximum.

## The web page

- Forgotten password, or the log line with the generated one is gone (a
  container that started long ago): set your own. In a container, put it in
  the environment variable `TCPRAID_UI_PASSWORD` (the compose file and the
  templates have the entry) and create the container again (`docker compose
  up -d`; a plain restart keeps the old environment); for a binary, use
  that variable or `-ui-password-file FILE`. It replaces the old password
  (`reference-files.md`). Or, for a new generated one: stop the node, delete
  `ui.password` in the data directory, start the node; the new password is
  printed once in the log.
- "too many wrong passwords from this address": wait a minute.
- Logged out again and again with two nodes on one host: both use the same
  cookie name. Reach each under a host name of its own.
- The page does not open from another machine: the API listens on
  `127.0.0.1` unless `-api` says otherwise.

## The node does not start

- "the data directory ... is in use by another tcpraid": one node per
  directory; the first one is still running.
- "... has a state file but neither identity.key nor identity.crt: its
  identity is lost, and a new one is not created in its place": restore the
  files from your backup, or start with an empty directory and pair again.
- `tcpraid status` says "the node is not running or -data DIR is not its data
  directory": the commands find the node through its data directory; give the
  same `-data` as the node has (in the container: `-data /data`).
- The log warns when the data directory or its key files can be read or
  written by others; fix the permissions.

## Numbers for monitoring

`GET /metrics` gives counters in the Prometheus text format (with the token):

```
# TYPE tcpraid_sessions_total counter
tcpraid_sessions_total 6
# TYPE tcpraid_sessions_aborted counter
tcpraid_sessions_aborted 0
# TYPE tcpraid_sessions_active gauge
tcpraid_sessions_active 1
```

`tcpraid status` exits with an error when no node answers; the container's
health check uses it.
