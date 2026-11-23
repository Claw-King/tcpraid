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
