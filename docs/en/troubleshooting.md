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
- Look at the session (Sessions page, `GET /api/v1/sessions`): `subs` should
  be the number of connections of the paths (8 by default), and
  `resent_bytes` a small share of `tx_bytes`. Many bytes sent again point to
  a path that loses or stalls; a second path with the roles `repair` and
  `control`, or UDP feedback, is what helps there (`concepts.md`).
- The speed of the stretch between the application and its node, and between
  the far node and the destination, counts as well: tcpraid cannot be faster
  than the slower of them, or than the application itself.
- When a session ends, both nodes log two lines about it: bytes, chunks sent
  again, sub-connections replaced, time stalled.

## The web page

- Forgotten password: stop the node, delete `ui.password` in the data
  directory, start the node; a new password is printed once in the log. Or
  set one with `TCPRAID_UI_PASSWORD` or `-ui-password-file`.
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
