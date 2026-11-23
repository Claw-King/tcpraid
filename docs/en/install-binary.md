# A node from the binary

For a plain Linux host, a router or a gateway. tcpraid is one static binary
with no runtime dependencies.

## Get the binary

Build it (Go 1.27 or newer):

```
go build -trimpath -o tcpraid ./cmd/tcpraid
```

or build the three release binaries (amd64, arm64, ARM v7) with
`deploy/build.sh 0.1.0`; its header shows how to do that without Go installed.
Copy the one for your machine to `/usr/local/bin/tcpraid`.

```
$ tcpraid version
tcpraid 0.1.0 (wire protocol 1)
```

(A binary from the plain `go build` above reports `0.1.0-dev`; the build
script puts in the version it is given.)

## Start a node

By hand, to try it:

```
nas-a# tcpraid run -listen 0.0.0.0:9851 -advertise nas-a.example.org:9851
... web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
... web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
... node 0.1.0 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
```

- `-listen` is where other nodes connect. Give it on the node that the other
  one can reach; the other node needs none.
- `-advertise` is the address the other node will use for it. It goes into
  the pairing codes this node makes.
- The data directory is `/var/lib/tcpraid` unless `-data` says otherwise. It
  holds the node's private key: keep it, and back it up like one.
- The web page and the API are at `127.0.0.1:9850`, reachable from the host
  itself only. To open the page from another machine, add `-api
  ADDRESS:9850` -- it is plain HTTP, so only on a network you trust.

With systemd: `deploy/tcpraid-node.service` is a unit for this. Copy it to
`/etc/systemd/system/`, adjust the command line in it (on a node that the
other one connects to, add `-advertise HOST:9851`: `tcpraid pair create` puts
that address into the code, and without one it needs `-addr` each time), then
`systemctl enable --now tcpraid-node`. The generated password is in
`journalctl -u tcpraid-node`.

```
nas-a$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.1.0 (wire protocol 1)
uptime       1s
sessions     0
peers        0
rules        0
```

The commands talk to the node on the same host through its data directory, so
run them as a user who can read it (root, with the unit as it is), and add
`-data DIR` if the node does not use the default.

## Pair the two nodes

```
nas-a$ tcpraid pair create
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node: tcpraid pair join (it then reads the code from standard input)
It is shown this once and is a secret until it is used.
tcpraid-pair-1-ueazax3dhku3...

nas-b$ tcpraid pair join
tcpraid-pair-1-ueazax3dhku3...        <- pasted; then Enter and Ctrl-D
paired with nas-a (fingerprint e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86)
```

`pair join` reads the code from standard input, so it does not show in the
process list or stay in the shell's history. Both nodes now list each other:

```
nas-b$ tcpraid peers
NAME   STATE   FINGERPRINT           PAIRED                PATHS
nas-a  online  e5645f9b...7795ec86   2026-01-15T10:00:00Z  nas-a.example.org:9851 (tls, data)

nas-b$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok
```

(Fingerprints are shortened in the table here.)

## Make a rule

On the node next to the application that starts the transfer:

```
nas-b$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:9801 -dest 192.0.2.10:873
rule rsync added: listening

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:9801  nas-a  192.0.2.10:873  0         listening
```

Connections to port 9801 of `nas-b` now come out at `nas-a` and go to
`192.0.2.10:873`, an address as `nas-a` sees it.

A rule on `nas-a` towards `nas-b` works too, although `nas-a` has no path to
`nas-b`: it asks `nas-b` to open the connections (`concepts.md`).

## On a router: transparent rules

A node on the router or gateway of a site can carry connections without the
applications knowing: the applications keep their normal addresses, and the
router redirects the connections to the node.

1. Start the node with a transparent listener:
   `tcpraid run -transparent 0.0.0.0:9800 ...`
2. Redirect the connections to it. tcpraid does not change the host's packet
   filter; you add the rule. With nftables, for connections that pass through
   the router from the LAN interface:

   ```
   table ip tcpraid {
     chain pre {
       type nat hook prerouting priority dstnat; policy accept;
       iifname "lan0" ip daddr 192.0.2.10 tcp dport 873 redirect to :9800
     }
   }
   ```

   `examples/redirect.nft` also covers connections made on the router itself.
   This needs root on the router.
3. Make a transparent rule that says which original destinations go to which
   peer:

   ```
   gw-b$ tcpraid rules add site-a -peer gw-a -match 192.0.2.10/32:873
   rule site-a added: listening

   gw-b$ tcpraid rules
   NAME    TAKES                            PEER  DESTINATION      SESSIONS  STATE
   site-a  redirected to 192.0.2.10/32:873  gw-a  (as redirected)  0         listening
   ```

   `-match` takes networks with an optional port or range of ports, separated
   by commas: `192.0.2.0/24:873,192.0.2.0/24:50000-50010`.

The first transparent rule that matches takes the connection; a redirected
connection that no rule matches is reset. The connection arrives at the same
address at the peer's end, so that address must mean the same machine there.

What was run for this document: a redirect of locally made connections (the
`output` hook) to a node with a transparent rule, 20 MiB through it with equal
checksums at both ends. The `prerouting` form above, for connections passing
through a router, is the one in `examples/redirect.nft` and was not run for
this version.
