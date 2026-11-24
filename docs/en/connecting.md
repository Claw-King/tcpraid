# Connecting two sites

Before a rule can carry anything, one node has to reach the other. This
document says what that takes, in which order to try the ways of getting it,
and which ways leave tcpraid with nothing to add. `tunnels.md` covers the
settings of a path in detail, while this document is about choosing the way.

## What tcpraid needs

tcpraid has two requirements, and a public IP address is only one way to meet
the first.

**One node can open TCP connections to the listener of the other.** One
direction is enough, and so is one port, the `-listen` port of the listening
node (9851 in the examples). It does not matter which of the two nodes
listens. Rules work on either node, because a node that cannot reach its peer
asks the peer to open the connections. The address that is connected to does
not have to belong to the listening node either. It can be a port that a
router forwards, the node's address inside an overlay network, or a port on a
third machine that passes the connections on, called a forwarding host below.
So a public address at one site is only one of the ways to get this.

**The connections stay separate on the way.** A session is carried by several
TCP connections, 8 on a path by default. tcpraid's gain comes from each of
them handling its own lost packets. If something on the way merges all of
them into a single connection, they share that connection's reaction to lost
packets, and tcpraid adds little or nothing to what a single connection does.
Where the single connection is a TCP connection, one lost packet holds up all
of them together. The data still arrives and is still correct, but the
transfer is no faster or steadier. This is where the tools differ, and the
table below says which does what.

## In which order to try

### 1. IPv6 at one site

A site without a public IPv4 address may still have public IPv6 addresses, so
look at the addresses of the machine the node runs on. If one site has them
and the other site can make outgoing IPv6 connections, no third machine and
no other program is needed.

- On the listening site's router, add an inbound rule that allows TCP to the
  node's IPv6 address on port 9851. IPv6 needs no address translation, so
  the rule only has to let the port in.
- Start that node with a listener that takes IPv6, and tell it the address
  the other node is to use:

  ```
  nas-b# tcpraid run -listen '[::]:9851' -advertise '[2001:db8:1::10]:9851'
  ```

- Pair as usual, with `tcpraid pair create` on `nas-b` and
  `tcpraid pair join` on `nas-a`. The path that `nas-a` gets points at that
  address:

  ```
  nas-a$ tcpraid peers check nas-b
  default  [2001:db8:1::10]:9851 (tls): ok
  ```

Use an address of the machine that stays the same, not a temporary one, or
use a host name that follows the address. `-advertise` and the address of a
path accept a name as well as an address. Nothing passes through a third
machine.

### 2. An overlay network that connects the two machines directly

Install the same overlay network on both machines, for example Tailscale,
ZeroTier or a WireGuard tunnel of your own. Each machine then has an address
inside the overlay. Give the listening node's overlay address when you make
the pairing code:

```
nas-b$ tcpraid pair create -addr 198.51.100.20:9851
```

Here `198.51.100.20` stands for the address of `nas-b` in the overlay. Check
two things before you rely on it:

- **That the overlay connects the two machines directly**, and has not fallen
  back to a relay. The table below says how each tool shows it. Check again
  when a transfer is slower than it was.
- **How fast the overlay itself is** between the two machines. When connected
  directly, these overlays carry their traffic in UDP datagrams. Measure the
  speed of the overlay first, with any tool you like, before you put tcpraid
  on it. tcpraid cannot carry more than what it runs on.

Keep the tier of the path at `tls` unless you have a reason to change it.
`tunnels.md` says when `off` is appropriate.

### 3. A machine with a public address that passes the connections on

If neither works, a rented machine with a public address can sit in between
as a forwarding host. The listening node's site keeps something connected to
the forwarding host, and the other node connects to a port on it.

- **frp**, set up as its documentation describes so that the node's port
  9851 appears as a port of the forwarding host. Set
  `transport.tcpMux = false` in **both** `frps.toml` and `frpc.toml`. With
  the default (`true`), frp's documentation says that all connections to the
  same `frpc` are multiplexed into the same TCP connection, which is the
  case tcpraid gains nothing from. Leave `transport.protocol` at its default,
  `tcp`.
- **A WireGuard tunnel from each site to the forwarding host**, with the host
  routing between the two tunnels. WireGuard carries IP packets in UDP, so
  each of tcpraid's connections remains a TCP connection from node to node.
- A plain forwarded port on the forwarding host is enough only where the host
  itself can reach the listening node, for instance through a tunnel that the
  node's site keeps open to it.

The path of the dialing node then points at the forwarding host. Give that
address when you pair, with `tcpraid pair create -addr vps.example.org:19851`
on the listening node, or set it afterwards:

```
nas-a$ tcpraid peers path set nas-b default -addr vps.example.org:19851
```

What this way costs:

- Every byte goes into the forwarding host and out of it again. Its bandwidth
  is the ceiling, and the whole transfer counts against a traffic allowance,
  twice where both directions are counted.
- Keep the tier at `tls`, the default. The two nodes then encrypt from one to
  the other, and the forwarding host passes on data it cannot read.

## More than one way at a time

Two of the ways above can be set up together, so that one takes over when
the other fails.

**Several addresses on one path.** A path can name up to 8 addresses of the
same listener, and the dialing node tries them in order whenever it opens a
connection. Put the address you prefer first, for example the IPv6 address,
and the forwarding host after it:

```
nas-a$ tcpraid peers path set nas-b default -addr [2001:db8:1::10]:9851,vps.example.org:19851
```

A pairing code that carries several addresses gives the new path all of
them. An address that does not answer costs up to 8 seconds for each
connection that tries it, until one address has worked. The node then
begins with that one. `tcpraid peers check` shows how each address does.
All the addresses of a path share its settings, so they must lead to the
same listener in the same security tier. Sessions that are running keep the
connections they have.

**A path in each direction.** When each site can be reached from the other,
give each node a path to the other one. `tcpraid peers path set` adds the
path on the node that created the pairing code, which has none after the
pairing. Each node then opens the connections for the rules it holds, as
before. While a node cannot open connections over its path and the other
node is still connected to it,
the other node opens the connections of new sessions instead. The node
changes back by itself when its path works again.

A node notices that its path is down by the control connection it keeps
over it. With several paths to the peer, that is the first path with the
control role, or the first path if none has that role, and the other paths
are not watched. The path counts as down once that connection has broken
and cannot be opened again. When the connections on the path are cut and
new ones are refused, that takes about ten seconds at most. When the path
stops carrying
traffic without refusing anything, it can take up to a minute, and a
connection that arrives in that time waits 30 seconds and is reset.
Sessions that are running are not moved. A session that was using the
broken path waits for it, as it does on a node with one path only. A rule
with `-paths` or `-subs` keeps to its node's paths.

**Choosing the dialing node.** A rule can say which node opens the
connections, with `-dial` (`reference-cli.md`). `self` and `peer` fix the
choice. `both` lets the two nodes open sub-connections for the same session,
each over its paths. That is useful when the two directions are
different lines, for example an IPv6 address at one site and a forwarded port
at the other. The session then uses both lines, and it keeps running on one
while the other is down. When both directions run over the same line, `both`
gives the session twice as many sub-connections on that line and nothing
else.

`both` needs a path on each node, and version 0.6.0 or later on both. When
one of the two paths is not there, the connections of the rule are reset and
the rule's state says which path is missing. A path that is there and down
is another matter. The session then starts on the sub-connections of the
other node. A peer whose path is down is asked again every 20 seconds, and
the node with the rule keeps dialing over its own path, until the missing
sub-connections arrive.

## Spare connections

A session whose connections the peer opens starts later than one that the
node dials for itself. The request goes over the control connection first.
Then the peer connects, and the two nodes authenticate before the node can
send the header of the session. On a long line these steps take several
round trips.

A node that holds no path to its peer therefore asks the peer to keep a few
connections ready. They are opened and authenticated ahead of any session
and stay idle until one needs them. A new session begins on one of them
immediately and asks for its other sub-connections as before. The peer then opens
a new spare connection to replace it.

Both nodes need version 0.6.0 or later, and each keeps to its `-spares`
(`reference-files.md`). The node without a path asks for that many, the
dialing node keeps at most that many, and the default is 2. `-spares 0` on
either node switches them off. The spare connections go over the first path
that carries data, and a rule with `-paths` or `-subs` does not use them. A
node takes a spare connection only from a peer that it asked, and only
after the connection has passed the same authentication as every connection
between the two nodes. `tcpraid peers show` says how many are waiting.

## Tool by tool

Only tools whose behaviour we could read in their own documentation are
listed. For any other tool, use the check in the next section.

| Way | Do the connections stay separate? | What to set, what to look at |
|---|---|---|
| A public address, or a port that a router forwards | yes | nothing |
| frp | only with `transport.tcpMux = false` in `frps.toml` and in `frpc.toml` | the default is `true`, and then all connections share one TCP connection between `frpc` and `frps` |
| SSH port forwarding (`ssh -L`, `ssh -R`) | no | every forwarded connection is a channel inside the one SSH connection. Do not use it for a path |
| WireGuard | yes | it carries IP packets in UDP, and each TCP connection inside deals with its own losses. Measure the tunnel's own speed first |
| Tailscale | when the two machines are connected directly | `tailscale status` (or `tailscale ping`) shows `direct` for the other machine once the two have exchanged traffic. `relay` means the traffic goes through one of Tailscale's DERP relay servers, which Tailscale's documentation describes as having limited quality of service, so do not run large transfers over it. `peer-relay` is another kind, a relay on a machine of your own network, covered in Tailscale's documentation |
| ZeroTier | when the two machines are connected directly | In `zerotier-cli peers`, `RELAY` on the line of the other machine means its packets go through ZeroTier's root servers because no direct connection could be made (the examples in ZeroTier's documentation show `DIRECT` otherwise). `zerotier-cli info` showing `TUNNELED` means it has fallen back to the TCP relay, which ZeroTier's documentation calls slow |
| Cloudflare Tunnel | no | `cloudflared` talks to Cloudflare's network over its own QUIC or HTTP/2 connections, and what it forwards does not travel as separate TCP connections. Do not use it for a path |

## How to see whether the connections are separate

Start with a measurement. `tcpraid peers test` sends test data over each
path, once with one sub-connection and once with all of them, and prints both
speeds:

```
nas-a$ tcpraid peers test nas-b
path test with nas-b: two runs of up to 10 seconds for each path
PATH     SUB-CONNECTIONS  ONE ALONE  ALL TOGETHER  TIMES
default  8                2.6 MB/s   19.8 MB/s     7.6
default: 8 sub-connections together carried 7.6 times what one carried alone.
```

When all of them together carry little more than one alone, there are two
explanations, and the test cannot tell which applies:

- **One connection already fills the line.** Compare `ONE ALONE` with what
  the line is known to carry. If the two are close, nothing is wrong. On
  such a line a single connection does well, and tcpraid has little to add.
- **Something on the path puts the connections into one.** `ONE ALONE` is
  then well below what the line carries. Go through the table above for the
  tool you use, and count the connections as described below.

The test sends real data and runs only when you start it. `reference-cli.md`
has its options.

`tcpraid sessions` shows, under `SUBS`, how many connections the node holds
for a session. That is what the node opened, not what crosses between the
sites. To see more, count TCP connections while a transfer runs.

At the listening node:

```
nas-b$ ss -Htn state established '( sport = :9851 )' | wc -l
9
```

On a system with BusyBox, use `netstat -tn | grep -c ':9851 .*ESTABLISHED'`.
Between two nodes that connect directly, and while one transfer runs, the
count is the `SUBS` of the session plus one for the control connection that
the two nodes keep. With several sessions it is their sum plus one. We have
run this much ourselves.

With a forwarding host, the listening node still sees one connection per
sub-connection, coming from whatever passes them on. With frp that is the
`frpc` at its own site, and with a tunnel it is the other node's address in
the tunnel. The
count that matters is the one between the sites. So count there, on the
forwarding host, the TCP connections that arrive from the other site or leave
for it. We expect about as many as at the node if the connections are passed
on one by one, and a single one if they are multiplexed. We have not run this
through each of the tools above.

A tunnel that carries packets in UDP has no TCP connections to count on the
way. In that case, rely on the tool's own status, direct or relayed, as in
the table.

## Sources

What this document says about other programs comes from their own
documentation. Defaults change from one version to the next, so check the
documentation of the version you run.

- frp, README, "TCP Stream Multiplexing":
  https://raw.githubusercontent.com/fatedier/frp/dev/README.md
  and the example configuration:
  https://raw.githubusercontent.com/fatedier/frp/dev/conf/frpc_full_example.toml
- The SSH connection protocol, RFC 4254 (channels; TCP/IP port forwarding):
  https://www.rfc-editor.org/rfc/rfc4254.txt
  and the SSH transport protocol, RFC 4253 (it runs on top of TCP):
  https://www.rfc-editor.org/rfc/rfc4253.txt
- WireGuard: https://www.wireguard.com/ and
  https://www.wireguard.com/known-limitations/
- Tailscale, "Connection types":
  https://tailscale.com/docs/reference/connection-types
- ZeroTier, "TCP Relay": https://docs.zerotier.com/relay/ ,
  the command line: https://docs.zerotier.com/cli/ ,
  and "Relaying": https://docs.zerotier.com/faq/relaying/
- Cloudflare Tunnel, "Tunnel run parameters":
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/
  and "Useful terms":
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/tunnel-useful-terms/
- QUIC, for a connection that is not TCP: RFC 9000 (streams) and RFC 9002
  (one congestion controller for a path):
  https://www.rfc-editor.org/rfc/rfc9000.txt
  https://www.rfc-editor.org/rfc/rfc9002.txt
