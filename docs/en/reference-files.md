# The node's options and files

## Options of `tcpraid run`

| Flag | Default | Meaning |
|---|---|---|
| `-data DIR` | `/var/lib/tcpraid` on Linux; see "The data directory" for macOS and Windows | data directory; created if missing (mode 0700) |
| `-api ADDR` | `127.0.0.1:9850` | listen address of the local API and the web page (plain HTTP, or HTTPS with `-api-tls`) |
| `-api-tls` | off | serve the local API and the web page over TLS, with a certificate the node makes for itself and keeps in the data directory. The commands on the host follow by themselves (`security.md`) |
| `-api-cert FILE`, `-api-key FILE` | none | a certificate and its key (PEM) to serve the local API and the web page with over TLS, in the place of the node's own |
| `-name NAME` | the name it has | name of the node. Given, it renames the node. Not given, the node keeps its name. At the first start it takes the host name and adds a hyphen and four random characters, as in `nas-k3x9`, so that two machines with the same host name do not become two nodes of the same name. A name is 1 to 64 bytes of printable characters, with no space at either end |
| `-listen ADDR` | off | listen address for other nodes (TLS). Without it the node can join another node but cannot be joined |
| `-advertise HOST:PORT` | none | where other nodes reach `-listen`; goes into the pairing codes this node makes. May be given more than once |
| `-listen-plain ADDR` | off | listen address for peers without TLS, for paths of the tier `off` |
| `-cert FILE`, `-key FILE` | none | a certificate and its key (PEM) for peers that use the `owncert` tier; needs `-listen`. The node logs the fingerprint of the certificate's key at start |
| `-transparent ADDR` | off | listen address for connections that the host redirects to the node; transparent rules decide where they go. Linux only |
| `-redirect MODE` | off | the node installs the redirect rules for its transparent rules with the `nft` program of the host, and removes them when it stops. `MODE` is `forwarded`, `local` or `both`. Linux only, with `-transparent`. See "Redirect rules installed by the node" |
| `-redirect-print` | off | with `-redirect`: the node logs the rules it would give `nft`, at the start and whenever its rules change, and installs none |
| `-udp-listen ADDR` | off | listen address (UDP) for feedback from peers that are told to send it here |
| `-ui-password-file FILE` | none | file with the password of the web page |
| `-no-ui` | off | switches the web page off. Its files and the login answer `404` and no password is made. The API with its token works as before |
| `-spares N` | `2` | spare connections, 0 to 8. A node asks a peer that it holds no path to for this many, and keeps at most this many to a peer that asks. `0` switches them off (`connecting.md`, "Spare connections") |
| `-metrics ADDR` | off | listen address for the node's figures: the Prometheus text format at `/metrics` and JSON at `/metrics.json`, read-only and without the token (see "The figures and the hook") |
| `-hook COMMAND` | none | a program that the node runs when a peer comes online or goes offline, a session begins or ends, or a connection of a rule fails (see "The figures and the hook") |
| `-hook-timeout DURATION` | `30s` | how long a run of `-hook` may last before the node ends it. `1s` to `1h` |
| `-stats DURATION` | off | log the node's totals this often, and one line for each session that runs then (`troubleshooting.md` explains the lines) |
| `-window SIZE` | `128MB` | per session: how much data this node holds that the peer has not yet reported received, and the space it offers the peer for data that arrives out of order. Whole MB |
| `-budget SIZE` | `256MB` | all sessions together: the most data held unacknowledged, and as much again held out of order |
| `-grace DURATION` | `900s` | how long a session is kept while nothing arrives from the peer |
| `-hol DURATION` | `1s` | a missing chunk is sent again once later data has waited for it longer than this plus the allowed queue (`-target`, or `-pigtarget` for a while after reports stalled). Each further attempt for the same chunk waits as long again |
| `-target DURATION` | `300ms` | the allowed queue of a sub-connection: how much data it may hold on the way beyond what its feedback loop covers, counted as time at the rate it delivers. `20ms` to `5s` |
| `-pigtarget DURATION` | `1s` | the allowed queue for 10 seconds after the peer's reports stalled for over a second. `20ms` to `30s` |
| `-dead DURATION` | `20s` | a sub-connection that has data on the way and makes no progress for this long, or that takes no write for this long, is closed and replaced. `5s` to `10m` |
| `-kickratio NUMBER` | `5` | a busy sub-connection that delivers less than the median of its path divided by this number, for 10 seconds, is replaced. `1.5` to `100` |
| `-kickfloor SIZE` | `256KB` | no sub-connection is replaced for being slow while the median rate of its path is below this many bytes a second. `1KB` to `1GB` |
| `-share` | on | each sub-connection is held to its share of the window, by the rate it delivers at. `-share=false` switches that off |
| `-fbsubs N` | `3` | while a session has neither a control sub-connection nor UDP feedback, each report on what arrived goes out on this many data sub-connections. `1` to `16` |
| `-fbevery DURATION` | `20ms` | how often a report on what arrived is sent while something changes. One goes every second in any case. `5ms` to `1s` |
| `-coalesce DURATION` | `20ms` | how long the node waits for more data from the application before it sends a chunk that is not full. `0s` to `1s` |
| `-timeline DURATION` | off | log one line of figures per session this often (`troubleshooting.md` explains the line) |
| `-maxrss MB` | off | stop the node when its resident memory exceeds this many MB. Linux only |

The start-up lines of the log repeat what is in effect:

```
node 0.11.2 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
transport window=128MB budget=256MB grace=20m0s hol=1s timeline=off maxrss=off
tuned target=500ms
```

The line `transport` always has these six fields. The line `tuned` is there
only when one of the settings from `-target` to `-coalesce` is not at its
default, and it lists those. A node that holds settings which were changed
while it ran logs them in a third line.

### The transport settings

The options from `-window` to `-maxrss` change how the transport works at
this node. The defaults suit most paths. Change one when a measurement says
so. `troubleshooting.md` explains the figures a node logs about a session,
and the table at the end of this section says which setting belongs to which
figure.

Each setting acts at this node's end of a session. Nothing of it is sent to
the peer, with one exception. A node's `-window` is what it sends with and
what it offers when it receives, and the other node learns it when a session
begins. The two nodes of a pair may run with different values.

A session takes each setting from three places, in this order:

| Layer | Set with | Holds for |
|---|---|---|
| rule | `tcpraid rules add NAME ... -SETTING VALUE`, `tcpraid rules set NAME -SETTING VALUE` | the connections of that rule |
| peer | `tcpraid peers set NAME -SETTING VALUE` | the sessions with that peer, whichever of the two nodes has the rule |
| node | `tcpraid run -SETTING VALUE` at the start, `tcpraid transport set -SETTING VALUE` while the node runs | every session of the node |

A setting that the rule does not have comes from the peer, and one that the
peer does not have comes from the node. A rule's settings act at the node
where the rule is stored. The other node of the same session takes its own,
from what it has stored for its peer and from its node. `-budget` and `-timeline`
belong to all sessions of a node together and exist for the node only.
`-maxrss` and `-stats` are set when the node starts and at no other time.

A session takes its settings when it begins and keeps them until it ends. A
change, in any of the three places, acts on the sessions that begin
afterwards. `tcpraid transport`, `tcpraid peers set` and `tcpraid rules set`
are in `reference-cli.md`.

A setting given on the command line of `tcpraid run` holds for as long as
that node runs, and `tcpraid transport set` refuses to change it. A setting
changed with `tcpraid transport set` is stored in `state.json` and holds
after a restart, unless the command line of the next start gives the same
setting.

Since version 0.8.0 the web page shows the node's settings under Settings,
with where each value comes from, and changes them there like
`tcpraid transport set`. A setting that the command line gives cannot be
changed in the page either. The settings of a peer and of a rule are shown
with the peer and the rule and are changed with the commands. An edit of a
peer or a rule made in the web page leaves its transport settings as they
are.

- A size is written `256MB`, `64KB` or `1GB`, or as a plain number of bytes.
  A duration is written `3600s`, `500ms`, `15m`.
- `-window` is a whole number of MB, at least 1 MB. `-budget` is at least
  1 MB. `-grace` and `-hol` are above zero. The table above has the ranges
  of the others.
- `-maxrss` is at least 2 x `-budget` + 100 MB, because a node holds up to
  twice `-budget` of stream data in normal operation. Past the limit the node
  stops as it does on a signal. It tells its peers, resets the application
  connections it carries, and exits with status 1 and the reason in its log.
  A container with a restart policy then starts it again, but the sessions
  are not resumed. The option also sets the soft memory limit of the Go
  runtime to 70% of the value. A budget that was stored while the node ran
  and does not fit the `-maxrss` of a later start keeps the node from
  starting. The message says so, and `-budget` on the command line goes
  before the stored value.
- A value that cannot work ends `tcpraid run` immediately, before it listens
  anywhere, with a message that names the option.
- The chunk size, `-chunk` in the manual mode, is not a setting of a node. It
  stays at 256 KB at every node, because the two ends of a session should
  use the same.

What each setting changes, and the figure in the log that belongs to it:

| Setting | A larger value | Look at it when |
|---|---|---|
| `-window` | lets the node hold more data that is sent and not yet reported received, and offers the peer more room for data that arrives out of order | `pumpwait` is near 1 while `unsent` is near 0 (`troubleshooting.md`) |
| `-budget` | lets all sessions together hold more | several sessions run at the same time and their windows add up to more than the budget |
| `-grace` | keeps a session longer while nothing arrives from the peer | the line is down for longer than 15 minutes at a time and the application should not be cut off, or a session should give up sooner |
| `-hol` | waits longer before a missing chunk is sent again | `holage` at the receiving node stays above zero. A shorter `-hol` at the sending node sends the chunk again sooner, and some chunks then go twice |
| `-target`, `-pigtarget` | lets each sub-connection hold more data on the way, so more waits behind a sub-connection that stalls | `cap=A/N` shows most sub-connections at their limit, or `peer wait` in the end line is long, since that wait is `-hol` plus this |
| `-dead` | keeps a sub-connection longer that makes no progress | the end line counts sub-connections that ended as `silent` or `write` on a path that pauses for long and then goes on |
| `-kickratio` | replaces fewer sub-connections for being slow | `kicks` in the end line is high on a path whose sub-connections differ in speed all the time |
| `-kickfloor` | replaces none for being slow up to a higher speed of the path | the same, on a slow path |
| `-share` | (a switch) with it off, a slow sub-connection may hold as much of the window as a fast one | you compare the two |
| `-fbsubs` | sends each report on more data sub-connections | no path has the control role and no UDP feedback is set up, and `fb gap max` in the end line is long |
| `-fbevery` | sends fewer reports, and the sending node learns later what arrived | reports take a noticeable share of a slow return direction |
| `-coalesce` | waits longer to fill a chunk | an application sends small pieces and waits for an answer to each. `0s` sends each piece without waiting |

### Redirect rules installed by the node

A transparent rule takes the connections that the host redirects to the
node's transparent listener. Without `-redirect` you write the redirect
rules of the host yourself (`install-binary.md`). With `-redirect`, since
version 0.9.0, the node writes them for its enabled transparent rules and
keeps them in line when those rules change:

```
# tcpraid run -transparent :9800 -redirect forwarded ...
# tcpraid rules add lan -peer nas-b -match 198.51.100.0/24:5000
# nft list table inet tcpraid
table inet tcpraid {
	chain prerouting {
		type nat hook prerouting priority dstnat - 10; policy accept;
		ip daddr 198.51.100.0/24 tcp dport 5000 redirect to :9800
	}
}
```

| `MODE` | Connections that are redirected | Chain |
|---|---|---|
| `forwarded` | those that pass through this host on their way, as on a router | hook `prerouting` |
| `local` | those that programs on this host open | hook `output` |
| `both` | both kinds | both chains |

- **One table, and nothing else.** The node writes the table `tcpraid` of
  the family `inet` and reads or changes no other table. At a start it
  replaces a table of that name that is already there. When it stops it
  deletes the table. A node that is killed cannot do that: the table stays,
  and the connections it redirects find no listener until the node runs
  again or you delete the table with `nft delete table inet tcpraid`.
- **Where the node's chains stand among others.** They have the priority
  -110. The `nft` manual says that chains of one hook are traversed in
  ascending order of priority, and gives -100 as the standard priority of
  destination NAT (`dstnat`), so the node's chains come before chains at
  that standard priority.
- **The listener has to be where the connections arrive.** The rules use
  the `redirect` statement, which the `nft` manual describes as a form of
  destination NAT that "always translates the destination address to the
  local host's one". A transparent listener on every address, as in
  `-transparent :9800`, receives them.
- **The node's own connections.** With `local` and `both`, the first rule of
  the `output` chain lets pass every connection of a process that runs as
  the same user as the node. Without it, a connection that the node opens
  to a destination could be redirected back to the node. Programs that run
  as that same user are therefore not redirected either. Run the node as a
  separate user, as the systemd unit in `deploy/` does, when programs on
  the host are to be redirected.
- **What it needs.** Linux, the program `nft` in the node's `PATH`, and the
  capability `CAP_NET_ADMIN`. Without one of them a node that is started
  with `-redirect` ends with a message that says which is missing. The
  container image holds no `nft`. The systemd unit in `deploy/` gives the
  node no capability at all, so add `CAP_NET_ADMIN` to the unit's two
  capability lines before you use `-redirect` with it.
- `-redirect-print` shows what the node would give `nft` without installing
  anything, which needs neither `nft` nor the capability.

### The figures and the hook

**`-metrics ADDR`** serves the node's figures on an address of their own.
That address answers `GET /metrics` in the Prometheus text format and
`GET /metrics.json` with the same samples as JSON, and nothing else. It asks
for no token. The API keeps its own `/metrics`, which needs the token like
every path of the API.

```
$ curl -s http://127.0.0.1:9900/metrics
# TYPE tcpraid_sessions_total counter
tcpraid_sessions_total 1
# TYPE tcpraid_bytes_tx counter
tcpraid_bytes_tx 50331648
...
# TYPE tcpraid_peer_online gauge
tcpraid_peer_online{peer="nas-b"} 1
# TYPE tcpraid_rule_listening gauge
tcpraid_rule_listening{rule="zfs"} 1
# TYPE tcpraid_rule_sessions gauge
tcpraid_rule_sessions{rule="zfs"} 0
```

| Figure | Meaning |
|---|---|
| `tcpraid_sessions_total`, `tcpraid_sessions_aborted`, `tcpraid_sessions_active` | sessions created, sessions that were cut off, sessions that run now |
| `tcpraid_subs_up`, `tcpraid_subs_lost` | sub-connections that came up, and sub-connections that were lost |
| `tcpraid_kicks_slow`, `tcpraid_kicks_silent`, `tcpraid_kicks_write` | sub-connections replaced for being slow, for making no progress, for taking no write |
| `tcpraid_bytes_tx`, `tcpraid_chunks_tx`, `tcpraid_resend_chunks`, `tcpraid_resend_by{cause=...}` | stream data sent for the first time, in bytes and in chunks, and chunks queued to be sent again, in all and by cause |
| `tcpraid_unacked_mb`, `tcpraid_rss_mb`, `tcpraid_runtime_memory_mb` | data held unacknowledged by all sessions, the memory of the process as the system counts it (Linux only), and the memory the program has from the system |
| `tcpraid_peer_online{peer=...}` | 1 while a control connection with the peer is up, else 0 |
| `tcpraid_rule_listening{rule=...}`, `tcpraid_rule_sessions{rule=...}` | 1 while the rule takes connections, and the sessions it carries now |
| `tcpraid_hook_running`, `tcpraid_hook_runs`, `tcpraid_hook_events_dropped`, `tcpraid_hook_runs_failed`, `tcpraid_hook_runs_timed_out` | only with `-hook`: runs that go on now, runs started, events not given to the hook, runs that failed, runs that were ended |

The rest are further counters of the transport.

The figures are numbers. The names of the peers and of the rules are in
them, as the values of the labels `peer` and `rule`. No address is in them,
no destination, no key or token, and no text of a failure. Whoever can reach
the address can read them, so leave the option off where the names alone say
too much. The node answers four requests for the figures at one time and
tells a fifth to come back, and a request changes nothing in the node. On an
address that is not loopback the node logs at its start that the figures
there need no token.

**`-hook COMMAND`** names a program that the node runs when something
happens. `COMMAND` is the program itself, a path or a name that is looked up
in `PATH`. It is executed directly, without a shell, and it gets no
arguments. A command that cannot be run stops the node at its start. What
happened is given to the program twice: as environment variables, and as
one line of JSON on its standard input.

| Event (`TCPRAID_EVENT`) | When | Fields |
|---|---|---|
| `peer_online` | the first control connection with a peer is up | peer |
| `peer_offline` | the last control connection with a peer is gone | peer |
| `session_start` | a session was created at this node | peer, rule, session, role, destination |
| `session_end` | a session ended at this node | the same, and tx_bytes, rx_bytes, clean, reason |
| `rule_failure` | a connection of a rule, or of `tcpraid connect`, was not carried | rule, reason |

| Variable | JSON field | Content |
|---|---|---|
| `TCPRAID_EVENT` | `event` | the event |
| `TCPRAID_TIME` | `time` | when it happened, RFC 3339 |
| `TCPRAID_PEER` | `peer` | the name of the peer |
| `TCPRAID_RULE` | `rule` | the name of the rule. It is not set for a session that the peer brought, and for `tcpraid connect` |
| `TCPRAID_SESSION` | `session` | the id of the session, 32 hex digits. `tcpraid sessions` shows the first six |
| `TCPRAID_ROLE` | `role` | `client` where this node took the application's connection, `server` where the peer brought the session |
| `TCPRAID_DESTINATION` | `destination` | where the connection goes at the far node |
| `TCPRAID_TX_BYTES`, `TCPRAID_RX_BYTES` | `tx_bytes`, `rx_bytes` | stream data sent and received by this node |
| `TCPRAID_CLEAN` | `clean` | `true` when the stream ended completely in both directions, `false` when it was cut off |
| `TCPRAID_REASON` | `reason` | how the session ended, as the log has it, or why the connection was not carried |

```
{"event":"session_end","time":"2026-01-15T10:00:48.232907258Z","peer":"nas-b","rule":"zfs","session":"7dc9fd4e6c7faf75936b2d4c871cf2d4","role":"client","destination":"192.0.2.10:5000","tx_bytes":50331648,"rx_bytes":0,"clean":true,"reason":"done"}
```

A variable that does not belong to an event is not set. A reason can hold
text that the peer sent. It reaches the program only in these values and
never on a command line, and in the variable the control characters 0 to 31
and 127 are spaces. The program gets the environment of the node without
the node's own variables that begin with `TCPRAID_`. No value holds a key, a
token or a pairing code.

The hook runs beside the node and cannot hold it up. At most four runs go on
at one time. An event that happens while four run is not given to the hook.
The node counts it and logs how many were left out. So a hook sees every
event only while it is quicker than the events come, and a program that must
not miss one reads the sessions and the peers from the API as well. A run
that lasts longer than `-hook-timeout` is ended. That ends the program the
node started, and not the programs which that one started in its turn, so a
script should not leave any behind. The status a run returns is
logged when it is not 0 and changes nothing in the node. Runs can overlap
and can end in another order than their events happened. Sessions of a path
test are not told.

### The exported configuration

`tcpraid config export` writes this file, and `tcpraid config apply` reads
it (`reference-cli.md`):

```
version: 1
node:
  name: nas-a
  fingerprint: 2b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfe
  transport:
    hol: 500ms
peers:
  - name: nas-b
    fingerprint: 1979efc900f372edcde2db07d83fe6eb000973864bde0d5d2c12ab194950edf9
    allow: 192.0.2.10/32:5000
    udp_dial: ""
    transport:
      window: 8MB
    paths:
      - name: default
        type: tcp
        addr: nas-b.example:9851
        security: tls
        roles:
          - data
        subs: 8
rules:
  - name: zfs
    peer: nas-b
    listen: 127.0.0.1:5000
    destination: 192.0.2.10:5000
    enabled: true
    subs: 3
```

| Field | Content |
|---|---|
| `version` | the version of this format, `1` |
| `node.name`, `node.fingerprint`, `node.transport` | the name of the node, the fingerprint that says which node the file was exported from, and the transport settings stored in the node while it ran, by name |
| `peers[].name`, `fingerprint` | which peer. Apply changes a peer only where both fit one that the node is paired with |
| `peers[].allow`, `udp_dial`, `transport` | the allow list, where UDP feedback for the peer is sent, and the peer's transport settings |
| `peers[].paths[]` | `name`, `type`, `addr`, `addrs`, `proxy`, `security`, `verify`, `pin`, `roles`, `subs`, `mark`, as `tcpraid peers path set` takes them. `proxy` is written without its user and password, and `proxy_login` is a note that the node has them |
| `rules[]` | `name`, `peer`, one of `listen` with `destination`, `match` or `socks`, then `enabled`, `paths`, `subs`, `dial` and `transport`, as `tcpraid rules add` takes them. A rule that does not say `enabled` is enabled |

A list or a setting that is empty is left out. A field that this version
does not know is an error when the file is applied.

### The password of the web page

Where it comes from, in this order:

1. `-ui-password-file FILE`: the content of the file, without the line end.
2. The environment variable `TCPRAID_UI_PASSWORD`, which is convenient where
   a container is configured in a web form. An empty value counts as not set.
3. The password used before. The data directory keeps a salted hash of it.
4. With none of these at the first start, the node makes a random password
   and writes it to its log once.

```
web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
```

A password given by file or variable replaces the stored one and keeps working
after the file or variable is taken away again. To get a new random password,
stop the node, delete `ui.password` and start it without the two.

A node started with `-no-ui` has no web page and so no password. It makes
none, ignores the variable, and refuses `-ui-password-file`. A `ui.password`
that an earlier start left in the data directory stays there unused.

## The data directory

On Linux the default is `/var/lib/tcpraid`. On macOS and Windows it is a
directory `tcpraid` in the user's configuration directory
(`~/Library/Application Support/tcpraid`, `%AppData%\tcpraid`). If the system
names no such directory, `-data` has to be given. On Windows the modes shown
below do not apply. Access to the files is what the folder's own permissions
say, and the node does not warn about them.

Owner, size and date are left out below.

```
$ ls -la /var/lib/tcpraid
drwx------  .
-rw-------  api.addr
-rw-r--r--  api.crt
-rw-------  api.key
-rw-------  api.tls
-rw-------  api.token
-rw-r--r--  identity.crt
-rw-------  identity.key
-rw-------  lock
-rw-------  state.json
-rw-------  ui.password
```

| File | Content |
|---|---|
| `identity.key` | the node's Ed25519 private key (PKCS#8, PEM). Created at the first start and never replaced; a key file that cannot be read is an error |
| `identity.crt` | a self-signed certificate for that key |
| `state.json` | node name, peers with their pair secrets and paths, unused pairing codes (id and a hash), rules. Written by the program, not by hand |
| `api.token` | access token of the local API |
| `ui.password` | a salted hash (PBKDF2-HMAC-SHA256) of the password of the web page; never the password |
| `api.addr` | the address the API is reached at from this host, written at each start; the commands read it |
| `api.key`, `api.crt` | only after a start with `-api-tls`: an ECDSA P-256 private key (PKCS#8, PEM) and a self-signed certificate for it, which the API and the web page are served with. The key is created once and never replaced. The certificate is valid for 800 days and is made again for the same key at a start that finds less than 30 days left, or a name missing. The host name is one of the names, so a container without a fixed host name gets a new certificate, for the same key, whenever it is made again |
| `api.tls` | only while the node was last started with TLS for the API: the certificate that the API presents, the node's own, or the certificate of `-api-cert`. The commands read it and talk only to an API that holds its key. A start without TLS removes the file |
| `lock` | empty; locked by the node while it runs |

- **One node runs on a data directory at a time.** A second `tcpraid run` on
  the same directory exits with an error and changes nothing. Do not delete
  the `lock` file while a node runs. It is not a leftover, and without it a
  second node could start on the directory.
- **The directory is the node.** Its fingerprint stays the same for as long as
  `identity.key` is kept, so keep the directory when you replace the program
  or the container. A directory that still has its `state.json` but has lost
  both identity files is refused, because the node does not make itself a new
  identity.
- **Back it up like a private key**, and never run two nodes from copies of
  one directory.
- `state.json` has a format version. A newer program reads the file of an
  older one and writes it in its own format the next time it changes. A file
  it cannot make sense of is refused and left as it is. One case of that is a
  rule whose destination is a host name, which version 0.3.0 and older do not
  take. Remove such rules before you go back to one of those versions.
  Another case is what version 0.5.0 added: a path with several addresses or
  a mark, and a rule with `-paths` or `-subs`. Version 0.4.0 and older do not
  take a file that holds one of them, so take them off before you go back.
  The same goes for a rule with a `-dial` other than `auto`, which version
  0.5.0 and older do not take, and for a transport setting stored for the
  node, a peer or a rule, which version 0.6.0 and older do not take. Take
  such a setting off with the word `default` in the place of its value. A
  SOCKS5 rule is the newest case: version 0.8.0 and older do not take a file
  that holds one, so remove such rules before you go back.

## Ports at a glance

| Port (default or customary) | Set by | For |
|---|---|---|
| 9850 TCP | `-api` | web page and local API |
| 9851 TCP | `-listen` | other nodes, TLS |
| any TCP | `-listen-plain` | other nodes, tier `off` |
| any TCP | a rule's listen address | applications |
| any TCP, 1080 by custom | a SOCKS5 rule's address | applications that speak SOCKS5 |
| any TCP | `-transparent` | redirected connections |
| any UDP | `-udp-listen` | feedback from peers |
| any TCP | `-metrics` | the node's figures, without a token |

Only 9850 has a built-in default. Port 9851 and the range 50000-50010 for
rules are what the container image and the templates in `deploy/` use.

## Limits

| | |
|---|---|
| sessions a node carries for its rules | 64 |
| sessions a node is the exit for | 64 |
| rules | 256, each over at most 1024 ports |
| paths to one peer | 16, each with 1 to 64 sub-connections and up to 8 addresses |
| handshakes in progress with one peer | 32 |
| unused pairing codes' validity | 1 second to 30 days, default 1 hour |
| worst-case memory for stream data | twice `-budget`: about 512 MB with the default |
