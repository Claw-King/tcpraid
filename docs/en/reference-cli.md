# Command reference

```
tcpraid run     [flags]                       start a node
tcpraid status  [-json]                       the node on this host
tcpraid pair    create | list | revoke ID | join [CODE]
tcpraid peers   [list] | show NAME | check NAME | remove NAME [-with-rules]
tcpraid peers   test NAME [-seconds N] [-path PATH] [-json]
tcpraid peers   set NAME [-allow LIST] [-udp-dial HOST:PORT]
tcpraid peers   path set NAME PATH [flags] | path remove NAME PATH
tcpraid rules   [list] | add NAME [flags] | remove NAME | enable NAME | disable NAME
tcpraid sessions [-json] [-watch]             what the node carries right now
tcpraid version
```

A first argument that does not start with `-` names a command. Any other
command line is the manual mode (`manual-mode.md`). `tcpraid COMMAND -h` lists
a command's flags.

Every command except `run` and `version` talks to the node on the same host,
and reads the API's address and token from the node's data directory. So it
takes `-data DIR` and must run as a user who can read that directory. The
default is `/var/lib/tcpraid` on Linux, a directory `tcpraid` in the user's
configuration directory on macOS and Windows, and `/data` in the container
image. In a container the command is
`docker exec tcpraid /tcpraid COMMAND ... -data /data`.

## run

Starts a node. Its flags are in `reference-files.md`. They say where it
listens and set six values of the transport (`-window`, `-budget`, `-grace`,
`-hol`, `-timeline`, `-maxrss`). It stops on SIGINT or SIGTERM with exit status 0,
after telling its peers and resetting the application connections it carries.

## status

```
$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.6.0 (wire protocol 1)
uptime       1s
sessions     0
peers        0
rules        0
```

`-json` prints the API's answer instead. If no node answers, it says so and
exits with status 1.

## pair

| Command | Does |
|---|---|
| `pair create [-addr HOST:PORT]... [-ttl DURATION]` | makes a pairing code and prints it. `-addr`: where the other node reaches this one (default: the node's `-advertise`); may be given more than once, up to 8. `-ttl`: how long it is valid (default `1h`, at most 30 days) |
| `pair join [CODE] [-addr HOST:PORT]` | pairs with the node that made the code. Without `CODE` it reads the code from standard input. `-addr`: connect there instead of to the addresses in the code. The path to the new peer keeps every address the code names, with the one that answered first |
| `pair list` | the codes that have not expired |
| `pair revoke ID` | makes a code worthless |

```
$ tcpraid pair create
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node with "tcpraid pair join", which then reads the code from standard input.
It is shown this once and is a secret until it is used.
tcpraid-pair-1-ueazax3dhku3...

$ tcpraid pair list
ID        VALID UNTIL           USED
a101905f  2026-01-15T11:00:00Z  no

$ tcpraid pair revoke a101905f
pairing code a101905f revoked
```

Only the code itself goes to standard output. The lines about it go to
standard error, so a script can capture the code alone with
`CODE=$(tcpraid pair create)`.

A code given to `pair join` as an argument shows in the process list and stays
in the shell's history, which matters while the code is still valid.

## peers

```
$ tcpraid peers
NAME   STATE   FINGERPRINT                                                       PAIRED                PATHS
nas-a  online  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86  2026-01-15T10:00:00Z  nas-a.example.org:9851 (tls, data)

$ tcpraid peers show nas-a
name         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
paired       2026-01-15T10:00:00Z
addresses    nas-a.example.org:9851
state        online, runs 0.6.0, last heard 2026-01-15T10:00:05Z
spares       2 connections wait for a session to begin on them

PATH     TYPE  ADDRESS                 PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS  MARK
default  tcp   nas-a.example.org:9851  -      tls       -       -    data   8     -

$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok

$ tcpraid peers remove nas-b
peer nas-b removed. The other node is not told and still lists this one.
```

- `peers show NAME` has a `spares` line while spare connections with the
  peer wait for a session (`connecting.md`, "Spare connections"). Either
  node shows the same number.
- `peers check NAME` opens a connection on every path, in the path's tier,
  and asks the other node to prove that it holds the pairing. A path with
  several addresses is checked at each of them, one line for each. The
  command exits with status 1 if a path or an address fails, and its last
  line says whether every path still has an address that works. A node
  without a path to the peer has nothing to check.
- `peers remove NAME` ends the pairing on this node. The sessions with that
  peer are reset, in both directions, and its connections are refused from
  then on. The command then lists the rules that still name the peer. They
  stay, and a node that is paired with this one under the same name later
  takes them over. `-with-rules` removes them together with the peer.

```
$ tcpraid peers remove nas-a
peer nas-a removed. The other node is not told and still lists this one.
These rules still name nas-a: rsync, zfs
A node that is paired with this one under the same name later takes them over. To prevent that, remove them with "tcpraid rules remove".

$ tcpraid peers remove nas-a -with-rules
peer nas-a removed. The other node is not told and still lists this one.
The rules that named it were removed with it: rsync, zfs
```

### peers test

```
$ tcpraid peers test nas-a
path test with nas-a: two runs of up to 10 seconds for each path
PATH     SUB-CONNECTIONS  ONE ALONE  ALL TOGETHER  TIMES
default  8                2.6 MB/s   19.8 MB/s     7.6
default: 8 sub-connections together carried 7.6 times what one carried alone.
```

`peers test NAME` measures what the sub-connections add on each path that
carries data. The node sends test data to the peer, which discards it. Each
path gets two runs, the first with one data sub-connection and the second
with all the path has. The rates are what the peer reported as received,
averaged over the second half of each run, so the start of a connection does
not count.

- `-seconds N` sets how long each run lasts, from 1 to 60. The default is 10.
  A run also ends after 1 GiB. On a line with a long delay, a longer run
  gives a steadier number.
- `-path PATH` tests only the path of that name.
- `-json` prints the answer of `POST /api/v1/peers/NAME/test` instead
  (`reference-api.md`).

The test runs only when you start it, and it sends real data, which competes
with the transfers that run at the same time. Data goes from the node where
you run the command to the peer. To measure the other direction, run the
command on the other node. A node without a path to the peer can run it too.
The peer then opens the sub-connections, and the table lists the peer's
paths.

When all sub-connections together carry less than 1.5 times what one
carries, the command says so:

```
default: 8 sub-connections together carried little more than one alone. Either one connection already fills this line, or something on the path puts the connections into one. "Connecting two sites" in the documentation (docs/en/connecting.md) says how to tell them apart.
```

The command cannot tell these two cases apart. `connecting.md` describes how
to do that. The command exits with status 1 when a path could not be
measured, and when the test cannot start. A test needs version 0.3.0 or
later on both nodes, and a node runs one test at a time.

### peers path

`peers path set NAME PATH [flags]` changes the path called `PATH` of the peer
`NAME`, or makes it if the peer has none of that name. Only what is given
changes.

| Flag | Meaning | A new path gets |
|---|---|---|
| `-addr HOST:PORT[,HOST:PORT...]` | the address to dial: the peer's listener, or the entry that leads to it (a port forward, a tunnel); through SOCKS5, the address the proxy is to connect to. Up to 8 addresses, separated by commas, are tried in that order (see below) | (required) |
| `-type tcp\|socks5` | direct, or through a SOCKS5 proxy | `tcp` |
| `-proxy socks5://[user:pass@]host:port` | the proxy, with `-type socks5` | |
| `-security tls\|owncert\|off` | the tier | `tls` |
| `-verify pin\|ca` | with `owncert`: how the certificate is checked | `pin` |
| `-pin FINGERPRINT` | with `owncert` and `pin`: fingerprint of the certificate's key, 64 hex digits | |
| `-roles data,repair,control` | what the path carries; not `data` and `repair` together | `data` |
| `-subs N` | sub-connections, 1 to 64 | 8 |
| `-mark N` | the socket mark of the path's connections, for the routing rules of the host. Linux only, and the node needs the capability `CAP_NET_ADMIN`. `0`: none | none |

```
$ tcpraid peers path set nas-a side -addr 192.0.2.10:9851 -type socks5 -proxy socks5://user:pass@127.0.0.1:1080 -roles repair,control -subs 2
path side of nas-a added
PATH     TYPE    ADDRESS                 PROXY                    SECURITY  VERIFY  PIN  ROLES           SUBS  MARK
default  tcp     nas-a.example.org:9851  -                        tls       -       -    data            8     -
side     socks5  192.0.2.10:9851         socks5://127.0.0.1:1080  tls       -       -    repair,control  2     -

$ tcpraid peers path remove nas-a side
path side of nas-a removed
```

A peer has at most 16 paths. A path's name is up to 32 bytes without spaces. A
path that does not fit is refused and nothing changes. A proxy is shown
without its user and password.

The node changes that path in a single step. Two of these commands that
run at the same time for two paths of one peer both take effect, and so does
a `peers set` that runs beside them.

**Several addresses.** A path can name up to 8 addresses of the same
listener, for example its IPv6 address and then a forwarded IPv4 port. The
node tries them in the order given whenever it opens a connection, and gives
each at most 8 seconds. It remembers which one worked and begins there the
next time, until the node restarts. `-addr` always sets the whole list, so
one address alone takes the others away.

```
$ tcpraid peers path set nas-a default -addr [2001:db8::10]:9851,203.0.113.7:9851
path default of nas-a changed
PATH     TYPE  ADDRESS                               PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS  MARK
default  tcp   [2001:db8::10]:9851,203.0.113.7:9851  -      tls       -       -    data   8     -

$ tcpraid peers check nas-a
default  [2001:db8::10]:9851 (tls): FAILED: dial tcp [2001:db8::10]:9851: connect: network is unreachable
default  203.0.113.7:9851 (tls): ok
tcpraid peers check: 1 of 2 address(es) to nas-a failed. Every path still has an address that works.
```

**The mark.** A path with a mark sets it on every connection it opens, so
that the host's routing rules can send those connections out by another
way than the rest. Setting a mark needs the capability `CAP_NET_ADMIN`, and
since Linux 5.17 `CAP_NET_RAW` is enough as well. Without it the connections
of the path fail, and `peers check` says that the capability is missing. On macOS and
Windows a mark is refused when it is set.

### peers set

`peers set NAME [-allow LIST] [-udp-dial HOST:PORT]` changes two settings of
the peer `NAME`. Only what is given changes, and an empty value clears a
setting.
With neither flag the command changes nothing and fails (exit status 1).

| Flag | Meaning |
|---|---|
| `-allow LIST` | the destinations the peer may reach through this node: networks with an optional port or range, separated by commas. `""`: any destination. Give it on the node the peer's connections come out of (`security.md`) |
| `-udp-dial HOST:PORT` | where this node sends UDP feedback for sessions with the peer: the address of the peer's `-udp-listen`. `""`: none (`concepts.md`) |

```
$ tcpraid peers set nas-a -allow 192.0.2.10/32:873,192.0.2.10/32:50000-50010
nas-a may reach through this node: 192.0.2.10/32:873,192.0.2.10/32:50000-50010

$ tcpraid peers set nas-a -allow ""
nas-a may reach any destination through this node

$ tcpraid peers set nas-b -udp-dial 192.0.2.20:8446
UDP feedback for sessions with nas-b goes to 192.0.2.20:8446
```

`peers show NAME` lists both settings when they are set. The web page has
them under Peers > Details > Advanced.

## rules

```
$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:9801 -dest 192.0.2.10:873
rule rsync added: listening

$ tcpraid rules add zfs -peer nas-a -listen 0.0.0.0:50000-50002 -dest 192.0.2.10:50100-50102
rule zfs added: listening

$ tcpraid rules
NAME   TAKES                PEER   DESTINATION             SESSIONS  STATE
rsync  0.0.0.0:9801         nas-a  192.0.2.10:873          0         listening
zfs    0.0.0.0:50000-50002  nas-a  192.0.2.10:50100-50102  3         listening

$ tcpraid rules disable zfs
rule zfs: disabled

$ tcpraid rules enable zfs
rule zfs: listening

$ tcpraid rules remove rsync
rule rsync removed. Connections it carries now run to their end.
```

`rules add NAME -peer PEER` with either

- `-listen IP:PORT[-PORT] -dest HOST:PORT[-PORT]`: a fixed rule. The IP of
  `-listen` may be left out, as in `:9801`, which means every address. An
  IPv6 address goes in brackets. The two ranges have the same length, at most
  1024 ports, and are mapped port by port. Two rules cannot listen on the
  same port of the same address. The destination takes an IP address or a
  host name, as in `-dest nas.lan:873`. The destination is in the peer's
  network, so the peer looks the name up, once for every connection, and the
  connection goes to the first address that the peer's allow list for this
  node lets through. Both nodes need version 0.4.0 or later for a host name.
- `-match NETWORKS`: a transparent rule, for a node started with
  `-transparent`. The networks are in CIDR form with an optional port or
  range, separated by commas, as in
  `192.0.2.0/24:873,[2001:db8::/32]:5000-5010`.

`-disabled` stores the rule switched off. A node has at most 256 rules, and a
rule's name is up to 64 bytes without spaces.

Two more flags narrow what the connections of a rule use:

| Flag | Meaning |
|---|---|
| `-paths NAME[,NAME...]` | the connections of the rule use only these paths, of the paths between the two nodes that carry data. Without it they use every path |
| `-subs N` | a connection of the rule keeps at most `N` data sub-connections on each path, and never more than the path is set to. Without it, as many as the path has |

The names are the ones that the node holding the paths gives them. On that
node `tcpraid peers show` lists them, and a name that is not there is refused
when the rule is added. On a node whose peer holds the paths, the peer
checks the names when a connection arrives, and both nodes need version
0.5.0 or later. Sub-connections for control and repair are not affected. A
rule that is narrowed says so in its state:

```
$ tcpraid rules add pull -peer nas-a -listen 0.0.0.0:9802 -dest 192.0.2.10:873 -paths side -subs 3
rule pull added: listening, over the paths side, with at most 3 sub-connections on a path
```

`rules enable` and `rules disable` keep both settings. To change them,
remove the rule and add it again.

`-dial` says which node opens the connections between the two nodes for the
sessions of the rule:

| Value | Meaning |
|---|---|
| `auto` | the default. This node opens them when it holds a path to the peer and the path works. The peer opens them when this node holds no path, or when this node's path is down and the peer's is not |
| `self` | only this node. This node needs a path to the peer |
| `peer` | only the peer, also when this node holds a path. The peer needs a path to this node |
| `both` | both nodes, for the same session. Each node needs a path to the other, and both nodes need version 0.6.0 or later |

With `both`, a session has the sub-connections of this node's paths and
those of the peer's paths at the same time. When the paths of one node go
down, the session goes on over the sub-connections of the other node, and
the missing ones come back when the paths work again. `connecting.md` says
when that is useful.

When a rule lacks what its `-dial` needs, its connections are reset and the
state of the rule says what is missing. The node does not open them another
way instead.

With `-dial peer` the names of `-paths` are the peer's. With `-dial both`
each node takes the names that it has a path for, and a node that has none
of them fails the connection. A rule with a `-dial` other than `auto` says
so in its state, and `rules enable` and `rules disable` keep the setting:

```
$ tcpraid rules add sync -peer nas-a -listen 0.0.0.0:9803 -dest 192.0.2.10:873 -dial both
rule sync added: listening, both nodes open the connections
```

`STATE` is `listening`, `disabled`, or `FAILED:` with the reason the rule
could not listen. In that case `rules add` exits with status 1, and the rule
is stored and tried again whenever the rules change. After "last connection:" comes what
went wrong with the last connection the rule could not carry, and how long
ago:

```
$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:9801  nas-a  192.0.2.10:873  0         listening, last connection: peer "nas-a" could not connect to 192.0.2.10:873: connect: connection refused (12s ago)
```

When the peer could not connect to the destination, or its allow list does
not hold the destination, the peer reports why and the rule shows it. A peer of a version before 0.3.0
does not report it, and the rule then says to look at the peer's log. While
a connection waits because the peer does not answer, the rule says what the
attempts run into, without a time. The next connection of the rule clears
the state.

When both nodes hold a path to each other and this node's own path is down,
the peer opens the connections of new sessions for as long as that lasts
(`connecting.md`). The state of the rule then reads `listening, the peer
opens the connections while this node's path is down`. A rule with `-paths`
or `-subs` does not change over, because the names it holds are those of
this node's paths.

A rule whose destination is a host name also says when the name gave no
address: the peer does not know the name, the peer is not connected, its
allow list holds none of the name's addresses, or it runs a version that does
not look names up.

## sessions

```
$ tcpraid sessions
ID      ROLE    RULE   PEER   DESTINATION     AGE  SENT      RECEIVED  AVERAGE    NOW        SENT AGAIN  SUBS  NOT SENT YET  IN FLIGHT  STATE
4a5411  client  rsync  nas-a  192.0.2.10:873  6s   320.0 MB  0 B       53.3 MB/s  30.0 MB/s  0.0%        8     96.0 MB       28.0 MB    ok
```

One line for each application connection the node carries right now, or
`no sessions`. The command looks twice, a second apart, to work out the speed.

| Column | Meaning |
|---|---|
| `ID` | the first six digits of the session's id, as in the node's log |
| `ROLE` | `client`: one of this node's rules took the connection. `server`: this node is the exit, and the peer's rule took it |
| `RULE`, `PEER`, `DESTINATION` | the rule (at the `client` end), the peer at the other end, and where the connection goes |
| `AGE` | how long the session has run |
| `SENT`, `RECEIVED` | stream data sent to the peer and received from it |
| `AVERAGE`, `NOW` | speed of both directions together: since the session began, and over the last second |
| `SENT AGAIN` | data queued to be sent again, as a share of the data sent |
| `SUBS` | live sub-connections |
| `NOT SENT YET` | data read from the application that still waits for a sub-connection. A value that stays large means the application delivers faster than the line carries |
| `IN FLIGHT` | data that was sent and that the peer has not yet reported as received |
| `STATE` | `ok`, `connecting` while the peer has not answered on any sub-connection, or how long the session has waited for the peer |

`SENT` counts what has gone out to the peer. Some of it may still be on its
way, so it runs ahead of what the far end shows as `RECEIVED`. The command
cannot say how long a transfer still takes, because tcpraid does not know how
much the application is going to send.

When the line between the two nodes is down, the session is kept for the
grace time (`-grace`, 15 minutes by default) and goes on when the line is
back. `STATE` then shows how long nothing has arrived from the peer and how
much of the grace time is left:

```
ID      ROLE    RULE   PEER   DESTINATION     AGE    SENT     RECEIVED  AVERAGE   NOW       SENT AGAIN  SUBS  NOT SENT YET  IN FLIGHT  STATE
4a5411  client  rsync  nas-a  192.0.2.10:873  5m12s  2.10 GB  0 B       6.9 MB/s  0.0 MB/s  1.0%        0     104.0 MB      24.0 MB    waiting for the peer for 3m12s, gives up in 11m48s
```

When the time is up, the session ends and the application's connection is
reset. The session of a path test shows `(path test)` under `RULE` and
`DESTINATION`.

A session whose sub-connections were opened partly by this node and partly
by the peer says so at the end of `STATE`, as in `ok, 8 sub-connections
opened by this node and 8 by the peer`. That is a session of a rule with
`-dial both`. With `-json`, every session has `subs_dialed`, the number of
its sub-connections that this node opened.

`-watch` prints the table again every 2 seconds, each time under the time of
day, until you interrupt it. `-interval DURATION` sets another interval, from
`1s` to `1h`.

`-json` prints the answer of `GET /api/v1/sessions` instead, taken once
(`reference-api.md`).

## version

```
$ tcpraid version
tcpraid 0.6.0 (wire protocol 1)
```
