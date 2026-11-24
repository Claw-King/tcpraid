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
| `-udp-listen ADDR` | off | listen address (UDP) for feedback from peers that are told to send it here |
| `-ui-password-file FILE` | none | file with the password of the web page |
| `-no-ui` | off | switches the web page off. Its files and the login answer `404` and no password is made. The API with its token works as before |
| `-spares N` | `2` | spare connections, 0 to 8. A node asks a peer that it holds no path to for this many, and keeps at most this many to a peer that asks. `0` switches them off (`connecting.md`, "Spare connections") |
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
node 0.8.0 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
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
  such a setting off with the word `default` in the place of its value.

## Ports at a glance

| Port (default or customary) | Set by | For |
|---|---|---|
| 9850 TCP | `-api` | web page and local API |
| 9851 TCP | `-listen` | other nodes, TLS |
| any TCP | `-listen-plain` | other nodes, tier `off` |
| any TCP | a rule's listen address | applications |
| any TCP | `-transparent` | redirected connections |
| any UDP | `-udp-listen` | feedback from peers |

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
