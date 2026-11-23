# Command reference

```
tcpraid run     [flags]                       start a node
tcpraid status  [-json]                       the node on this host
tcpraid pair    create | list | revoke ID | join [CODE]
tcpraid peers   [list] | show NAME | check NAME | remove NAME
tcpraid peers   set NAME [-allow LIST] [-udp-dial HOST:PORT]
tcpraid peers   path set NAME PATH [flags] | path remove NAME PATH
tcpraid rules   [list] | add NAME [flags] | remove NAME | enable NAME | disable NAME
tcpraid sessions [-json]                      what the node carries right now
tcpraid version
```

A first argument that does not start with `-` names a command. Any other
command line is the manual mode (`manual-mode.md`). `tcpraid COMMAND -h` lists
a command's flags.

Every command but `run` and `version` talks to the node on the same host: it
reads the API's address and token from the node's data directory. So it takes
`-data DIR` (default `/var/lib/tcpraid`; in the container image `/data`) and
must run as a user who can read that directory. In a container:
`docker exec tcpraid /tcpraid COMMAND ... -data /data`.

## run

Starts a node. Its flags are in `reference-files.md`: where it listens, and
six settings of the transport (`-window`, `-budget`, `-grace`, `-hol`,
`-timeline`, `-maxrss`). It stops on SIGINT or SIGTERM with exit status 0,
after telling its peers and resetting the application connections it carries.

## status

```
$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.1.3 (wire protocol 1)
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
| `pair join [CODE] [-addr HOST:PORT]` | pairs with the node that made the code. Without `CODE` it reads the code from standard input. `-addr`: connect there instead of to the addresses in the code |
| `pair list` | the codes that have not expired |
| `pair revoke ID` | makes a code worthless |

```
$ tcpraid pair create
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node: tcpraid pair join (it then reads the code from standard input)
It is shown this once and is a secret until it is used.
tcpraid-pair-1-ueazax3dhku3...

$ tcpraid pair list
ID        VALID UNTIL           USED
a101905f  2026-01-15T11:00:00Z  no

$ tcpraid pair revoke a101905f
pairing code a101905f revoked
```

Only the code itself goes to standard output; the lines about it go to
standard error, so a script can capture the code alone
(`CODE=$(tcpraid pair create)`).

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
state        online, runs 0.1.3, last heard 2026-01-15T10:00:05Z

PATH     TYPE  ADDRESS                 PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS
default  tcp   nas-a.example.org:9851  -      tls       -       -    data   8

$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok

$ tcpraid peers remove nas-b
peer nas-b removed; the other node is not told and still lists this one
```

- `peers check NAME` opens a connection on every path, in the path's tier,
  and asks the other node to prove that it holds the pairing. It exits with
  status 1 if a path fails. A node without a path to the peer has nothing to
  check.
- `peers remove NAME` ends the pairing on this node: the sessions with that
  peer are reset, in both directions, and its connections are refused from
  then on.

### peers path

`peers path set NAME PATH [flags]` changes the path called `PATH` of the peer
`NAME`, or makes it if the peer has none of that name. Only what is given
changes.

| Flag | Meaning | A new path gets |
|---|---|---|
| `-addr HOST:PORT` | the address to dial: the peer's listener, or the entry that leads to it (a port forward, a tunnel); through SOCKS5, the address the proxy is to connect to | (required) |
| `-type tcp\|socks5` | direct, or through a SOCKS5 proxy | `tcp` |
| `-proxy socks5://[user:pass@]host:port` | the proxy, with `-type socks5` | |
| `-security tls\|owncert\|off` | the tier | `tls` |
| `-verify pin\|ca` | with `owncert`: how the certificate is checked | `pin` |
| `-pin FINGERPRINT` | with `owncert` and `pin`: fingerprint of the certificate's key, 64 hex digits | |
| `-roles data,repair,control` | what the path carries; not `data` and `repair` together | `data` |
| `-subs N` | sub-connections, 1 to 64 | 8 |

```
$ tcpraid peers path set nas-a side -addr 192.0.2.10:9851 -type socks5 -proxy socks5://user:pass@127.0.0.1:1080 -roles repair,control -subs 2
path side of nas-a added
PATH     TYPE    ADDRESS                 PROXY                    SECURITY  VERIFY  PIN  ROLES           SUBS
default  tcp     nas-a.example.org:9851  -                        tls       -       -    data            8
side     socks5  192.0.2.10:9851         socks5://127.0.0.1:1080  tls       -       -    repair,control  2

$ tcpraid peers path remove nas-a side
path side of nas-a removed
```

A peer has at most 16 paths. A path's name is up to 32 bytes without spaces. A
path that does not fit is refused and nothing changes. A proxy is shown
without its user and password.

### peers set

`peers set NAME [-allow LIST] [-udp-dial HOST:PORT]` changes two settings of
the peer `NAME`. Only what is given changes; an empty value clears a setting.
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
rule rsync removed; connections it carries now run to their end
```

`rules add NAME -peer PEER` with either

- `-listen IP:PORT[-PORT] -dest IP:PORT[-PORT]`: a fixed rule. The IP of
  `-listen` may be left out (`:9801`, every address); an IPv6 address goes in
  brackets. The two ranges have the same length, at most 1024 ports, and are
  mapped port by port. The destination is an IP address; host names are not
  supported yet. Two rules cannot listen on the same port of the same
  address.
- `-match NETWORKS`: a transparent rule, for a node started with
  `-transparent`. Networks in CIDR form with an optional port or range,
  separated by commas: `192.0.2.0/24:873,[2001:db8::/32]:5000-5010`.

`-disabled` stores the rule switched off. A node has at most 256 rules; a
rule's name is up to 64 bytes without spaces.

`STATE` is `listening`, `disabled`, or `FAILED:` with the reason the rule
could not listen (`rules add` then exits with status 1; the rule is stored and
tried again whenever the rules change). After "last connection:" comes what
went wrong with the last connection the rule could not carry.

## sessions

```
$ tcpraid sessions
ID      ROLE    RULE   PEER   DESTINATION     AGE  SENT      RECEIVED  AVERAGE    NOW        SENT AGAIN  SUBS
4a5411  client  rsync  nas-a  192.0.2.10:873  6s   320.0 MB  0 B       53.3 MB/s  30.0 MB/s  0.0%        8
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

`SENT` counts what has gone out to the peer; some of it may still be on its
way, so it runs ahead of what the far end shows as `RECEIVED`. The command
cannot say how long a transfer still takes: tcpraid does not know how much the
application is going to send.

`-json` prints the answer of `GET /api/v1/sessions` instead, taken once
(`reference-api.md`).

## version

```
$ tcpraid version
tcpraid 0.1.3 (wire protocol 1)
```
