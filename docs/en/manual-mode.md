# Manual mode

Without a subcommand, `tcpraid` runs in **manual mode**, which has no node, no
pairing and no web page. One process is the **client**, near the application,
and one is the **server**, near the destination. Everything is given by flags or a
configuration file. It is the lowest layer of the program, useful on small
routers, in scripts, and where you want to set the transport's parameters
yourself.

```
 app --> [client] ==== data sub-connections (TCP, any path) ====> [server] --> destination
            ^                                                        |
            +------- feedback: UDP / control connection / piggyback -+
```

- The client accepts the application's connection, opens the sub-connections
  and tells the server where the application wanted to go. It needs to be able
  to open TCP connections to the server, directly or through a relay, port
  forward or SOCKS5 proxy. The server never connects to the client.
- The server accepts sub-connections and opens the real connection to the
  destination, if its allow list permits.

**Manual mode does not encrypt the stream.** See "Security" below.

## Quick start

Create a key, the same file at both ends:

```
head -c 32 /dev/urandom | base64 > psk
```

Server (far end), allowed to reach 192.0.2.5 port 873 only:

```
tcpraid -mode server -listen 0.0.0.0:9901 -allow 192.0.2.5/32:873 -psk psk
```

Client (near end), forwarding to one fixed destination:

```
tcpraid -mode client -listen 127.0.0.1:9801 -remote server.example.org:9901 \
        -dst 192.0.2.5:873 -psk psk
```

Now connect the application to `127.0.0.1:9801` instead of `192.0.2.5:873`.

`tcpraid -check ...` validates flags and configuration file and exits:

```
$ tcpraid -check -mode client -listen 127.0.0.1:9801 -remote server.example.org:9901 -dst 192.0.2.5:873 -psk psk
ok: 0.5.0 client paths [data/data x8], window 128 MB, budget 256 MB, maxrss 0 MB, grace 15m0s, psk true, udp listen "" dial ""
```

## Transparent use

Without `-dst`, the client takes the original destination of each connection
redirected to it (Linux `SO_ORIGINAL_DST`, IPv4 and IPv6). This is for Linux
only. On macOS and Windows a client needs `-dst` and says so at its start. The application
keeps connecting to its normal address. With nftables, on the client host or a
router the traffic passes through (this needs root there):

```
table ip tcpraid {
  chain pre {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "lan0" ip daddr 192.0.2.5 tcp dport 873 redirect to :9801
  }
}
```

`examples/redirect.nft` also covers connections made on the router itself.

## Paths and roles

A client can use several paths at once. A path is a way to reach the server,
that is, an address, optionally through a SOCKS5 proxy. Each path has one or
more roles:

| Role | Carries | Default connections |
|---|---|---|
| `data` | new chunks, re-sent chunks, snapshots | 8 (`subs`) |
| `repair` | re-sent chunks only; new chunks only while no `data` sub-connection is up | 2 (`subs`) |
| `control` | snapshots only | 1 (`ctrl`) |

`data` and `repair` cannot share a path. A typical setup has a main path for
`data` and a second, independent path for `repair` and `control`. Holes are
then filled over the second path, feedback does not queue behind bulk data,
and the second path takes over when the first is down.

Paths are set in a configuration file (below). The flags describe the common
case, which is one data path (`-remote`, `-n`, `-socks`) and optionally
control connections (`-ctrl`, `-ctrlremote`, `-ctrlsocks`).

To send a path a particular way without extra privileges, point it at a proxy
or port forward whose destination is the tcpraid server. `mark` (SO_MARK) is
available for policy routing but needs CAP_NET_ADMIN. It exists on Linux
only, and elsewhere a path with a mark is refused at the start.

## Feedback channels

On some paths the reverse direction loses packets heavily while the forward
direction is busy. Plain TCP does not mind, because its ACKs are cumulative
and plentiful, but feedback sent as TCP data in that direction gets stuck
behind a lost segment for seconds or tens of seconds. tcpraid therefore sends its
feedback on every channel you give it and uses whichever arrives first:

| Channel | What it is | Use it when |
|---|---|---|
| UDP | One encrypted datagram per snapshot, never retransmitted | One end has a reachable UDP port (recommended) |
| `control` path | A TCP connection that carries no data, on a path of its own | You have a second path that the data does not load |
| piggyback | On the data sub-connections (always on; once a second while another channel works) | Fallback |

For UDP, the end with a reachable port listens and the other sends to it. The
listener answers to the source address, so the sending end may be behind NAT.
Both ends need the key.

```
client: ... -udplisten 0.0.0.0:8445
server: ... -udpdial client.example.org:8445
```

**Rule of thumb:** give tcpraid at least one feedback channel that does not
retransmit (UDP) or a path whose reverse direction is clean. With piggyback
only, a path with heavy reverse loss may run at half speed.

## Configuration file

`-config FILE` reads a YAML file. Flags given on the command line override it.
Unknown keys are errors.

```yaml
mode: client
listen: 0.0.0.0:9801
psk_file: /etc/tcpraid/psk
paths:
  - { name: main,   type: tcp, addr: 127.0.0.1:9821, roles: [data], subs: 8 }
  - { name: second, type: tcp, addr: 127.0.0.1:9822, roles: [repair, control], subs: 4 }
  - { name: proxy,  type: socks5, proxy: "socks5://user:pass@127.0.0.1:1080",
      addr: 192.0.2.10:9901, roles: [data], subs: 4 }
control:
  udp: { listen: 0.0.0.0:8445 }
send: { window: 128MB, target: 300ms }
grace: 900s
limits: { maxrss_mb: 700, maxsess: 64 }
```

```
$ tcpraid -config client.yaml -check
ok: 0.5.0 client paths [main/data x8, second/repair x4, second/control x1, proxy/data x4], window 128 MB, budget 256 MB, maxrss 700 MB, grace 15m0s, psk true, udp listen "0.0.0.0:8445" dial ""
```

`addr` is the address the client dials over that path. It is the tcpraid
server's listener, or the entry that leads to it. For a port forward or a
proxy inbound with a fixed destination it is that forward's address. The
server needs no `paths`:

```yaml
mode: server
listen: 0.0.0.0:9901
psk_file: /etc/tcpraid/psk
allow: [ "192.0.2.5/32:873" ]
control:
  udp: { dial: client.example.org:8445 }
```

`examples/` holds a systemd unit for the manual mode, the nftables redirect,
and client and server files for a two-path setup.

## Options

Sizes are plain bytes on the command line (`-window 134217728`) and take a unit
in the file (`128MB`). Durations use Go syntax (`300ms`, `15m`).

| Flag | File key | Default | Meaning |
|---|---|---|---|
| `-mode` | `mode` | | `client` or `server` |
| `-listen` | `listen` | | client: where applications connect; server: where sub-connections arrive |
| `-remote` | `paths[].addr` | | client: the server's address |
| `-n` | `paths[].subs` | 8 | data sub-connections per path |
| `-socks` | `paths[].proxy` | | reach the server through a SOCKS5 proxy |
| `-ctrl`, `-ctrlremote`, `-ctrlsocks` | `paths[]` with role `control` | 0 | control connections and how to reach the server for them |
| `-dst` | `dst` | | client: fixed destination instead of the redirected one |
| `-allow` | `allow` | any | server: destinations allowed: `CIDR`, `IPv4-CIDR:port[-port]`, `[IPv6-CIDR]:port[-port]` |
| `-psk` | `psk_file` | none | key file (16 bytes or more): MACs on headers and snapshots; required for UDP |
| `-udplisten` | `control.udp.listen` | | receive UDP feedback here |
| `-udpdial` | `control.udp.dial` | | send UDP feedback to the peer's listener |
| `-fbsubs` | `control.piggyback_subs` | 3 | without a control connection: data sub-connections each snapshot is sent on |
| `-window` | `send.window` | 128 MB | per session: data sent and not yet reported received; also the reorder space offered to the peer |
| `-budget` | `send.budget` | 256 MB | all sessions together: unacknowledged data sent, and (separately) out-of-order data held |
| `-target` | `send.target` | 300 ms | queue each sub-connection may hold beyond its feedback loop |
| `-pigtarget` | `send.stalled_target` | 1 s | the same for 10 s after feedback stalled |
| `-hol` | `send.hol` | 1 s | how long a missing chunk may hold up later ones before it is re-sent |
| `-chunk` | `send.chunk` | 256 KB | chunk size |
| `-grace` | `grace` | 900 s | keep a session whose peer is silent this long |
| `-dead` | `dead` | 20 s | replace a sub-connection with data in flight and no progress this long |
| `-kickratio` | | 5 | replace a busy sub-connection delivering below the median of its path divided by this for 10 s |
| `-maxrss` | `limits.maxrss_mb` | off | exit above this many MB; at least 2 x `budget` + 100 MB (checked at start). Linux only: refused at start on macOS and Windows, so leave `limits.maxrss_mb` out of a configuration file there |
| `-maxsess` | `limits.maxsess` | 64 | refuse new sessions beyond this many |
| `-timeline` | `timeline` | off | one diagnostic line per session this often |
| `-stats` | | off | log sessions, memory and per-sub-connection state this often |
| `-metrics` | `metrics` | off | expvar metrics at `http://ADDR/debug/vars` |
| `-check` | | | validate flags and file, print the result, exit |
| `-version` | | | print the version and wire protocol version |

`tcpraid -h` lists the remaining tuning and test flags.

**Memory.** In the worst case a process holds about twice `budget` of stream
data, which is about 512 MB with the defaults. Lower `budget` and `window` on small
machines.

## Operation

**Stopping.** On SIGINT / SIGTERM, `-maxrss` or `-life`, tcpraid tells every
peer first, then resets its application connections and exits. The peer drops
those sessions immediately instead of holding them, with their buffered data,
for `grace`.

**Restarts.** Session state is in memory. A process that restarts cannot resume
its sessions, so the application sees a reset and must retry.

**Observability.** `-timeline 1s` logs per session: MB/s sent (`tx`), re-sent
(`re`), acknowledged (`ack`), received (`rx`) and delivered (`dlv`), the
unacknowledged and reorder backlog, feedback age and the channels it arrived on
(`fbc` control, `fbu` UDP), sub-connections at their cap, the age of the oldest
chunk waiting for a missing one (`holage`), the share of time the reader waited
for window (`pumpwait`), and the feedback loop p50 / p95 in ms (`loop`). The
two numbers that reveal a feedback problem first are `pumpwait` and the loop
p95. `troubleshooting.md` explains every field of this line and of the two
lines a session logs at its end.

## Security

- Manual mode **does not encrypt the stream**. If the data needs protecting,
  run it over a path that is already encrypted, or use nodes (`tcpraid run`),
  which encrypt by default.
- Without `-psk`, anyone who can reach the server port can open connections to
  the allowed destinations. Always set `-allow`, and use a key on any path you
  do not fully trust.
- With a key, sub-connection headers and snapshots are authenticated
  (HMAC-SHA256, truncated to 16 bytes), and headers more than 10 minutes old
  are rejected. The clocks of the two ends must be within 10 minutes of each
  other.
- UDP feedback is encrypted and authenticated (AES-256-GCM, a key per session
  and direction). Datagrams that fail authentication are dropped and rate
  limited.

## Compatibility

A client and a server must run the same wire protocol version, and there is
no negotiation. A node and a manual-mode process do not talk to each other,
because connections between nodes start with an opening that manual mode does
not send.
