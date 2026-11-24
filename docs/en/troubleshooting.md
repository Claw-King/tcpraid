# Troubleshooting

Where to look first: the **rule's status** (Rules page, or `tcpraid rules`),
the **peer's state** (Peers page, or `tcpraid peers`), and the **node's log**
(`docker logs tcpraid`, `journalctl -u tcpraid-node`).

## The application cannot connect, or is cut off at once

A rule that cannot carry a connection resets it and keeps the reason, with
the time. So when a connection fails, look first at `tcpraid rules` on the
node with the rule:

```
nas-a$ tcpraid rules
NAME  TAKES         PEER   DESTINATION         SESSIONS  STATE
back  0.0.0.0:9811  nas-b  198.51.100.20:7100  0         listening, last connection: this node has no path to peer "nas-b", and the peer, which would open the connections, is not connected to this node. Start the peer, or add a path with "tcpraid peers path set".

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
taken  0.0.0.0:9700  nas-a  192.0.2.10:873  0         FAILED: listening on 0.0.0.0:9700 failed: listen tcp 0.0.0.0:9700: bind: address already in use
```

| What you see | What it means |
|---|---|
| `STATE` is `FAILED` with a reason | the rule could not listen: the port is taken, or not allowed. The other rules run. It is tried again when the rules change |
| "this node has no path to peer ... and the peer is not connected" | the peer is offline, and this node cannot dial it. Start the peer, check its path to this node |
| connection refused before any of this | nothing listens on that port. In a container: is the rule's port published, and does the rule listen on `0.0.0.0`? |
| "peer ... could not connect to ADDRESS: connect: connection refused" (or another error of the system) | the far node tried the destination and failed. Nothing listens there, or the address is wrong as the far node sees it |
| "peer ... refused the destination ADDRESS, which is not in the allow list it keeps for this node" | the far node's allow list for this node does not hold the destination. Change it on the far node with `tcpraid peers set` |
| "peer ... could not find the address of NAME: no such host" | the rule's destination is a host name, and the far node, which looks it up, does not know it. Check the name on the far node, or give the rule the IP address |
| "peer ... could not find the address of NAME: the lookup failed. The log of that node says why." | the far node's resolver gave an error other than "no such host", for example because its name server does not answer. The far node's log has the error |
| "peer ... does not look up host names for this node (it runs version ...)" | the far node is older than 0.4.0. Update it, or give the rule the IP address |
| "peer ... refused the destination NAME:PORT, because the name stands for no address in the allow list it keeps for this node" | the far node's allow list holds none of the addresses of the name, with that port. Change the list on the far node with `tcpraid peers set` |
| "peer ... did not say within 10s which address NAME stands for" | the far node did not answer the question in time. Try the connection again. If it keeps happening, look at the far node's log |
| "peer ... looks up as many host names as it does at a time, and did not look up NAME" | the far node is busy with other lookups. Try again in a moment |
| "peer ... is not connected to this node, so it cannot be asked which address NAME stands for" | a host name is looked up over the connection between the two nodes, which is down. `tcpraid peers` shows the peer as offline. See "The peer is offline" |
| "peer ... could not connect to ADDRESS. Its log says why." | the same two cases, with a far node of a version before 0.3.0, which does not report the reason. Its log has `dial ADDRESS: ... connection refused`, or `dial ADDRESS: not in the allow list of the peer` |
| reset after 15 seconds, rule on the node without a path | the peer was asked to connect and nothing arrived: the peer can no longer reach this node's listener. The same happens for up to 30 seconds after the peer lost its control connection where this node did not see it end, because the request goes out on the old connection |
| "peer ... does not answer on the connections this node opens", and after 30 seconds "peer ... answered on none of the connections within 30 seconds" | none of the sub-connections came up, and the text after it says what the attempts ran into. One cause: the pairing was ended on the other node. The rule then says "the other node does not accept this node, which is not paired with it or no longer is", and the other node's log has lines "refused a connection from ...: not a peer of this node". Pair again |
| "the connections to peer ... cannot be opened" with an error | this node cannot connect to the peer's listener at all. `tcpraid peers check NAME` tries each path |
| "none of the path's N addresses gave a connection", followed by each address and its error | the path names several addresses and none of them worked. `tcpraid peers check NAME` tries each address on its own |
| "the path's socket mark N could not be set, because the node lacks the capability for it" | the path has a mark and the node may not set one. Give the node the capability `CAP_NET_ADMIN`, or `CAP_NET_RAW` on Linux 5.17 and later, or take the mark off with `tcpraid peers path set NAME PATH -mark 0` |
| "the rule names the path NAME, and this node has no path of that name to peer ... that carries data" | the rule has `-paths` with a name that is no path for data on this node. `tcpraid peers show PEER` lists the paths |
| "peer ... could not open the connections: it has no path called NAME that carries data" | the same, where the peer holds the paths: the name in the rule's `-paths` is not one of the peer's paths for data. Look at the paths on the peer |
| "peer ... does not keep a rule to the paths it names (it runs version ...)" | the rule has `-paths` or `-subs`, the peer holds the paths, and the peer is older than 0.5.0. Update it, or add the rule without the two flags |
| "the rule lets only this node open the connections (dial self), and this node has no path to peer ..." | the rule has `-dial self` and this node holds no path. Add a path with `tcpraid peers path set`, or add the rule again with another `-dial` |
| "the rule lets only peer ... open the connections (dial peer), and the peer is not connected to this node" | the rule has `-dial peer` and no control connection with the peer is up. `tcpraid peers` shows the peer as offline. See "The peer is offline" |
| "the rule lets both nodes open the connections (dial both), which needs a path on each node, and ..." | the rule has `-dial both`, and the message goes on to say which node holds no path to the other, or none of the paths the rule names. Add the path on that node, or add the rule again with another `-dial` |
| "peer ... does not open connections of a session that the other node opens connections of too (it runs version ...)" | the rule has `-dial both` and the peer is older than 0.6.0. Update it, or use another `-dial` |
| "nothing arrived from peer ... for the whole grace time" | the line between the two nodes was down for longer than `-grace`. See the next section |
| "peer ... was restarted while it carried a connection of this rule, and the connection was reset" | the other node came back as a new process (it was restarted, killed, or ran out of memory) and this node reset the connection the old process had been carrying. Start the transfer again. If it keeps happening, look at the other node's log and at its memory limits (`-maxrss`, the limit of its container) |

A destination is an address **as the far node sees it**. With the far node in
a container, `127.0.0.1` is the container, not the NAS.

## A transfer has stopped moving

`tcpraid sessions` says whether the session is waiting for the line:

```
ID      ROLE    RULE   PEER   DESTINATION     AGE    SENT     RECEIVED  AVERAGE   NOW       SENT AGAIN  SUBS  NOT SENT YET  IN FLIGHT  STATE
4a5411  client  rsync  nas-a  192.0.2.10:873  5m12s  2.10 GB  0 B       6.9 MB/s  0.0 MB/s  1.0%        0     104.0 MB      24.0 MB    waiting for the peer for 3m12s, gives up in 11m48s
```

| `STATE` | What it means |
|---|---|
| `ok`, and `NOW` is zero | the two nodes still talk to each other, and no data moves. The application is not sending, or the destination is not reading |
| `waiting for the peer for ..., gives up in ...` | nothing has arrived from the peer for that long. The line is down, or the peer has stopped. The session is kept and goes on when the line is back. When the grace time is over, the application's connection is reset |
| `connecting` | the peer has not answered on any sub-connection yet. `tcpraid rules` says what the attempts run into |

`tcpraid sessions -watch` prints the table every 2 seconds. A longer outage
needs a longer `-grace` on both nodes (`reference-files.md`).

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
  address, a port forward on the way, or the peer's `-listen` port.
- `FAILED: tls: first record does not look like a TLS handshake`: the address
  leads to something that is not a node's TLS listener, for example the
  node's port 9850 instead of 9851.
- `FAILED: ... connection reset by peer. The other node closed the connection
  during the handshake and may be busy. Try again.`, every time: besides a
  busy node, this is what a path of the `tls` tier gets from a node's plain
  listener (`-listen-plain`). The message goes on to say so. Check that the
  port and the path's tier belong together.
- "the security tiers of the two ends do not match. This path is of the
  owncert tier, and the listener at ... has no certificate of its operator to
  show.": the path expects the other node's own certificate, and that node
  was started without `-cert` and `-key`. Start it with them, or set the
  path's security to `tls`.
- "the node at this address is not the peer. Its key has another
  fingerprint.": another node answers there, or the peer has a new identity
  because its data directory was lost.
- "the other node does not accept this node, which is not paired with it or
  no longer is": the pairing was ended on the other node.
- "the other end closed the connection without answering": on a path of the
  tier `off`, the other node is not paired with this one, or the address is
  the other node's `-listen`, which takes TLS only. The message names that
  second cause on such a path. In any tier, the two hold different pair
  secrets after a pairing that was cut off, or their clocks are more than ten
  minutes apart, in which case the other node's log says so. Pair them again,
  or set the clock.

A node without a path to its peer cannot check, and says `this node has no path to
... and can only be reached by it`. Check from the other side.

## Pairing fails

- "no address for the other node to connect to": the node does not know its
  outside address. Give one when making the code, with `-addr HOST:PORT` or
  the field in the web page, or start the node with `-advertise`.
- "the other node did not accept the pairing code": the code was used, has
  expired (one hour by default), was revoked or was not copied whole.
- "the pairing code is damaged": copy it again, complete and unchanged.
- "the node at this address is not the one that made the pairing code": the
  address leads to another node.
- A join that cannot finish is given up after 60 seconds.

## It works but it is not faster

- Measure what the sub-connections add with `tcpraid peers test NAME`
  (`reference-cli.md`). It sends test data over each path, once with one
  sub-connection and once with all of them. If all together are not clearly
  faster than one, there is nothing for tcpraid to gain on that path. Either
  one connection already fills the line, or something on the path puts the
  connections into one, and `connecting.md` says how to tell them apart.
  `iperf3` with one stream and with eight measures the same without tcpraid.
- Look at the session on the Sessions page, with `tcpraid sessions`, or with
  `GET /api/v1/sessions`. `subs` should be the number of connections of the
  paths (8 by default), and `resent_bytes` should be a small share of
  `tx_bytes`. The command shows that share in the column `SENT AGAIN`. Many
  bytes sent again point to a path that loses or stalls. A second path with
  the roles `repair` and `control`, or UDP feedback, is what helps there
  (`concepts.md`).
- A session of a rule with `-dial both` should show sub-connections from both
  nodes: its `STATE` in `tcpraid sessions` ends with how many each node
  opened. When it shows those of one node only, the path of the other node
  is down. When the peer could not open its share at the start of the
  session, the log of the node with the rule has the line "peer ... could
  not open its share of the connections of a session", with the reason the
  peer gave, and the peer is asked again every 20 seconds until its first
  connection arrives.
- The speed of the stretch between the application and its node, and between
  the far node and the destination, counts as well, because tcpraid cannot be
  faster than the slower of them, or than the application itself.
- When a session ends, both nodes log two lines about it: bytes, chunks sent
  again, sub-connections replaced, time stalled. A node started with
  `-timeline` logs the same kind of figures while the session runs. The next
  section explains them, and how they tell whether to change a transport
  setting such as `-window` or `-hol` (`reference-files.md`).

## Reading the session lines in the log

When a session ends, each node logs two lines about it. A node started with
`-timeline 2s` also logs one line per session every two seconds while it
runs, the first one about half a second after the session begins. The figures
are those of the node that logs them. The sending figures matter at the
sending end, and the receiving figures at the other.

### The timeline line

```
[ddc465] tl t=3.0 tx=34.2 re=0.0 ack=37.1 rx=0.0 dlv=0.0 unacked=0 unsent=0 pend=0 infl=0 sacked=63 fbage=0.03 fb=144/9 fbc=0 fbu=0 cap=0/8 holage=0.00 pumpwait=0.28 starve=0.94 loop=243/1521
```

Rates are MB/s over the time since the previous line, and sizes are MB at the
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
| `cap=A/N` | of the `N` live sub-connections that carry data (roles `data` and `repair`), `A` hold as much in flight as they are allowed. The allowance is what arrives within the feedback loop plus the allowed queue (`-target`) |
| `holage` | receiving: seconds the oldest data in `pend` has waited for a chunk that is missing before it |
| `pumpwait` | share of the time since the previous line that this node did not read from the application because `-window`, the peer's window or `-budget` was full. A wait is counted in the line during which it ends: one wait that lasts over several lines shows as 0 in them and then as a value above 1 |
| `starve` | share of the time an average data sub-connection had room and nothing to send. Sub-connections of a `repair` path count and are idle most of the time |
| `loop=P50/P95` | milliseconds from sending data on a sub-connection to the report that it arrived, median and 95th percentile. It includes the time the data queued on the way. A sub-connection that carries nothing new keeps reporting its last data, so the figure grows while sub-connections are idle (those of a `repair` path among them) |

What limits a transfer shows at the sending node:

- `pumpwait` near 0 while data moves (`tx` above zero): the node waits for
  the application, and the path is not the limit. A `pumpwait` of 0 with `tx`
  at 0 over several lines is one long wait that has not ended yet, so read it
  as one of the cases below.
- `pumpwait` near 1 with `unsent` large and every sub-connection of the data
  paths at its cap: the application delivers faster than the path takes. The
  window is full of data that has not been sent yet, so a larger `-window`
  holds more of it and sends no faster.
- `pumpwait` near 1 with `unsent` near 0: the data is out, and the window is
  full of what the peer has not reported or cannot deliver yet, with `sacked`
  large. Look at the receiving node. If its `holage` is above zero, data
  waits there for missing chunks. A larger `-window` at both nodes gives it
  more room, and a shorter `-hol` at the sending node sends a missing chunk
  again sooner, at the price of some chunks sent twice. The sending node sends
  a chunk again once later data has waited for `-hol` plus the allowed queue.
  While `holage` stays well below that, the chunks that are waited for arrive
  without being sent again, and a shorter `-hol` adds copies and no speed. If
  its `holage` stays
  at zero while its `pend` is large, the application there is slow to take
  the data, and no setting of tcpraid changes that. The `stall` figure in its
  end line says for how long. If its `holage` is zero and its `pend` is small
  as well, nothing waits there. The window is then smaller than what the path
  holds in flight, and a larger `-window` at both nodes may let more through.

Every setting named here can be changed while the node runs, with
`tcpraid transport set`, and for one peer or one rule alone, with
`tcpraid peers set` and `tcpraid rules set`. A change acts on the sessions
that begin afterwards, so start the transfer again to see what it does.
`reference-files.md` lists all the settings under "The transport settings",
each with the figure that belongs to it.

### The two lines at the end

```
[ddc465] session end: done (6.2s tx 200MB rx 0MB chunks 800 resends 0 kicks 0 subs 8 stall 0.0s)
[ddc465] session diag: pump win 0.7s | resent | subs died lost=0 silent=0 write=0 slow=0 | fb gap max 0.0s >1s 0 | gap repairs 0 (retries 0) ms p50/p95/max: peer wait 0/0/0, queued 0/0/0, to release 0/0/0 | snapshots truncated 0
```

`session end` (or `session aborted`) gives the reason. Two reasons are not
what they seem. `local read: the path test is over` is how the sessions of
`tcpraid peers test` end, and is no fault. `server could not reach ADDRESS`
also stands for a destination that the peer's allow list does not let
through, and `tcpraid rules` gives the reason then. After the reason come:

| Field | Meaning |
|---|---|
| first number | how long the session lasted |
| `tx`, `rx` | stream data sent (first time only) and received |
| `chunks` | chunks sent for the first time |
| `resends` | chunks queued to be sent again |
| `kicks` | sub-connections replaced for being much slower than the others of their path (`-kickratio`, `-kickfloor`) |
| `subs` | sub-connections that came up over the life of the session |
| `stall` | time spent in writes to the application that each took over 50 ms: the application, or the stretch to it, was slow to take the data |

`session diag`:

| Field | Meaning |
|---|---|
| `pump win` | total time this node did not read from the application because a window or the budget was full (the sum behind `pumpwait`) |
| `resent gap=N stall=N dead=N` | why chunks were queued again: the peer reported later data waiting for it (`gap`); it was not reported received long after it was due (`stall`); its sub-connection ended (`dead`). Causes with a count of zero are left out |
| `subs died lost / silent / write / slow` | how sub-connections ended: the connection failed or was closed (`lost`, which includes the ones the other node closes when the session finishes); no progress for the time of `-dead`, 20 seconds by default (`silent`); a write that did not finish in that time (`write`); replaced for being slow (`slow`) |
| `fb gap max`, `>1s N` | the longest time between two reports from the peer while data was unacknowledged, and how often it was over a second |
| `gap repairs N (retries R)` | chunks that were sent again because the peer reported them missing and that it then reported received (the count stops at 4096; `resent gap=` has them all), and how many sends for a missing chunk were not the first for it |
| `peer wait` | for those chunks: how long later data had waited at the peer when this node decided to send the chunk again. **It is not a property of the path.** The decision is taken once the wait is over `-hol` plus the allowed queue (`-target`, 300 ms by default, or `-pigtarget`, 1 s, for a while after reports stalled), so with the defaults it is never below 1.3 s and usually just above |
| `queued` | from that decision until the chunk was handed to a sub-connection |
| `to release` | from that decision until the peer reported the chunk received |
| `snapshots truncated` | reports this node built that could not list every range it holds out of order |
| `udp in / out / gap max` | with UDP feedback: datagrams accepted and sent for the session, and the longest time between two accepted ones |

The three times are in milliseconds: median, 95th percentile, maximum.

### The lines of `-stats`

A node started with `-stats 5m` logs its totals every five minutes, and after
them one line for each session it carries at that moment:

```
stats sessions=1 goroutines=28 rss=20MB heap=6MB unacked=0MB resends=0 kicks=0/0/0 rxdrop=0 udp in/out/bad/toobig/old=0/0/0/0/0
[8ac974] 127.0.0.1:7000 9s tx 242MB rx 0MB unacked 0MB sacked 128MB pending 0MB subs: [p0 2277/1812KB/s loop0ms infl0KB cap684KB] [p0 2624/776KB/s loop0ms infl0KB cap790KB]
```

The `stats` line. The counters count from the start of the node, and the
others are the state at that moment.

| Field | Meaning |
|---|---|
| `sessions` | sessions the node carries now |
| `goroutines` | goroutines of the process (it grows with sessions and sub-connections; a number that only ever grows is worth a report) |
| `rss` | resident memory of the process, what `-maxrss` is compared with. `n/a` on macOS and Windows, where the program does not read it |
| `heap` | memory the Go runtime has in use for the heap |
| `unacked` | data of all sessions that was read from applications and that the peers have not reported received; `-budget` limits it |
| `resends` | counter: chunks queued to be sent again |
| `kicks=A/B/C` | counters: sub-connections this node replaced because they were much slower than the others of their path (`A`), made no progress for the time of `-dead` (`B`), or did not finish a write in that time (`C`) |
| `rxdrop` | counter: chunks dropped on arrival because the space for data out of order was full; the peer sends them again |
| `udp in/out/bad/toobig/old` | counters for UDP feedback: datagrams accepted; sent; rejected (not for a session of this node, or failed authentication); reports that were too large for a datagram and went over the TCP connections only; authentic datagrams that were not newer than one already seen |

The line of a session:

| Field | Meaning |
|---|---|
| `[8ac974]` | the first six digits of the session's id |
| `127.0.0.1:7000` | the destination of the connection |
| `9s` | how long the session has run |
| `tx`, `rx` | stream data sent (first time only) and received, as in the end line |
| `unacked`, `sacked`, `pending` | the timeline's `unacked`, `sacked` and `pend` |
| `subs:` | one group for each live sub-connection |
| `p0` | the path it belongs to: the number of the path, from 0, in the list of paths that the dialing node keeps for the other |
| `2277/1812KB/s` | what the peer reported as delivered over this sub-connection: the highest rate over a second, and the average of the last two seconds |
| `loop0ms` | the shortest time seen on this sub-connection from sending data to the report that it arrived |
| `infl` | sent on this sub-connection and not yet reported received |
| `cap` | how much this sub-connection may hold in flight |
| `[ctrl p1]` | a sub-connection of a `control` path: it carries reports, no data |

At the receiving node, the rates and `infl` are zero, because it sends no
stream data.

## The web page

- Forgotten password, or the log line with the generated one is gone, as
  with a container that started long ago: set your own. In a container, put
  it in the environment variable `TCPRAID_UI_PASSWORD`, for which the compose
  file and the templates have the entry, and create the container again with
  `docker compose up -d`. A plain restart keeps the old environment. For a
  binary, use that variable or `-ui-password-file FILE`. It replaces the old
  password (`reference-files.md`). For a new generated one instead, stop the
  node, delete `ui.password` in the data directory and start the node. The
  new password is printed once in the log.
- "too many wrong passwords from this address": wait a minute.
- "another login from this address is being checked" and "too many logins are
  being checked at this moment": try again in a moment. The node checks one
  password at a time for one address and four in all.
- "a login needs the header X-Tcpraid-Request: 1": a program that logs in
  without a browser has to send that header since version 0.8.0.
- Logged out after an upgrade to 0.8.0: the cookie of a login has another
  name since then. Log in once more.
- The browser warns about the certificate: a node started with `-api-tls`
  serves the page with a certificate it made for itself. Its log has the line
  "the API at ... is served over TLS" with the SHA-256 fingerprint of the
  certificate. Compare that with the fingerprint the browser shows, and
  accept the certificate if they are the same.
- "the API at ... presents another certificate than api.tls holds", from a
  command: the command found a certificate at the address in `api.addr` that
  the node did not record, and sent nothing. Something else
  listens at that address, or one of the two files was replaced. The node
  writes both at each start, so start it again.
- The page does not open from another machine: the API listens on
  `127.0.0.1` unless `-api` says otherwise.

## The node does not start

- "the data directory ... is in use by another tcpraid": only one node runs
  on a directory, and the first one is still running.
- "... has a state file but neither identity.key nor identity.crt. Its
  identity is lost, and a new one is not created in its place.": restore the
  files from your backup, or start with an empty directory and pair again.
- `tcpraid status` says "the node is not running or -data DIR is not its data
  directory": the commands find the node through its data directory. Give the
  same `-data` as the node has, which in the container is `-data /data`.
- The log warns when the data directory or its key files can be read or
  written by others. Fix the permissions.
- "the budget of ... that is stored in the node does not fit: -maxrss ... is
  below 2 x budget + 100 MB ...": a budget was set with
  `tcpraid transport set`, and this start gives a `-maxrss` that is too low
  for it. Start with a higher `-maxrss`, or with `-budget`, which goes
  before the stored value.

## Numbers for monitoring

`GET /metrics` gives counters in the Prometheus text format and needs the
token:

```
# TYPE tcpraid_sessions_total counter
tcpraid_sessions_total 6
# TYPE tcpraid_sessions_aborted counter
tcpraid_sessions_aborted 0
# TYPE tcpraid_sessions_active gauge
tcpraid_sessions_active 1
```

A node started with `-stats DURATION` writes its totals to the log at that
interval, for a script that reads the log (see "The lines of `-stats`"
above).

`tcpraid status` exits with an error when no node answers, and the container's
health check uses it.
