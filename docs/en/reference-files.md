# The node's options and files

## Options of `tcpraid run`

| Flag | Default | Meaning |
|---|---|---|
| `-data DIR` | `/var/lib/tcpraid` | data directory; created if missing (mode 0700) |
| `-api ADDR` | `127.0.0.1:9850` | listen address of the local API and the web page (plain HTTP) |
| `-name NAME` | the name it has | name of the node. Given, it renames the node. Not given, the node keeps its name; at the first start it takes the host name. A name is 1 to 64 bytes of printable characters, with no space at either end |
| `-listen ADDR` | off | listen address for other nodes (TLS). Without it the node can join another node but cannot be joined |
| `-advertise HOST:PORT` | none | where other nodes reach `-listen`; goes into the pairing codes this node makes. May be given more than once |
| `-listen-plain ADDR` | off | listen address for peers without TLS, for paths of the tier `off` |
| `-cert FILE`, `-key FILE` | none | a certificate and its key (PEM) for peers that use the `owncert` tier; needs `-listen`. The node logs the fingerprint of the certificate's key at start |
| `-transparent ADDR` | off | listen address for connections that the host redirects to the node; transparent rules decide where they go |
| `-udp-listen ADDR` | off | listen address (UDP) for feedback from peers that are told to send it here |
| `-ui-password-file FILE` | none | file with the password of the web page |

The transport runs with its default settings in a node; the tuning options of
the manual mode do not apply.

The start-up line of the log repeats what is in effect:

```
node 0.1.0 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
```

### The password of the web page

Where it comes from, in this order:

1. `-ui-password-file FILE`: the content of the file, without the line end.
2. The environment variable `TCPRAID_UI_PASSWORD` (convenient where a
   container is configured in a web form). An empty value counts as not set.
3. The password used before: the data directory keeps a salted hash of it.
4. None of these, at the first start: the node makes a random password and
   writes it to its log once.

```
web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
```

A password given by file or variable replaces the stored one and keeps working
after the file or variable is taken away again. To get a new random password,
stop the node, delete `ui.password` and start it without the two.

## The data directory

(Owner, size and date left out.)

```
$ ls -la /var/lib/tcpraid
drwx------  .
-rw-------  api.addr
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
| `lock` | empty; locked by the node while it runs |

- **One node runs on a data directory at a time.** A second `tcpraid run` on
  the same directory exits with an error and changes nothing. Do not delete
  the `lock` file while a node runs: it is not a leftover, and without it a
  second node could start on the directory.
- **The directory is the node.** Its fingerprint stays the same for as long as
  `identity.key` is kept, so keep the directory when you replace the program
  or the container. A directory that still has its `state.json` but has lost
  both identity files is refused: the node does not make itself a new
  identity.
- **Back it up like a private key**, and never run two nodes from copies of
  one directory.
- `state.json` has a format version. A newer program reads the file of an
  older one and writes it in its own format the next time it changes; a file
  it cannot make sense of is refused and left as it is.

## Ports at a glance

| Port (default or customary) | Set by | For |
|---|---|---|
| 9850 TCP | `-api` | web page and local API |
| 9851 TCP | `-listen` | other nodes, TLS |
| any TCP | `-listen-plain` | other nodes, tier `off` |
| any TCP | a rule's listen address | applications |
| any TCP | `-transparent` | redirected connections |
| any UDP | `-udp-listen` | feedback from peers |

Only 9850 has a built-in default; 9851 and the range 50000-50010 for rules are
what the container image and the templates in `deploy/` use.

## Limits

| | |
|---|---|
| sessions a node carries for its rules | 64 |
| sessions a node is the exit for | 64 |
| rules | 256, each over at most 1024 ports |
| paths to one peer | 16, each with 1 to 64 sub-connections |
| handshakes in progress with one peer | 32 |
| unused pairing codes' validity | 1 second to 30 days, default 1 hour |
| worst-case memory for stream data | about 512 MB |
