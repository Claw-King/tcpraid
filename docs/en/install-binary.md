# A node from the binary

This document is for a plain Linux host, a router or a gateway, and for a Mac
or a Windows machine. tcpraid is one binary with no runtime dependencies, and
on Linux it is static. The steps below are written for Linux. The section "On
macOS and on Windows" further down says what differs there.

## Get the binary

**Download it.** Each release at
`https://github.com/Claw-King/tcpraid/releases` has the binaries and a file
with their SHA-256 sums. For Linux:

| Machine (`uname -m`) | File |
|---|---|
| `x86_64` | `tcpraid-0.2.0-linux-amd64` |
| `aarch64` | `tcpraid-0.2.0-linux-arm64` |
| `armv7l` | `tcpraid-0.2.0-linux-armv7` |
| | `tcpraid-0.2.0-SHA256SUMS` |

Download the one for your machine and the sums file into the same directory,
check the binary against the sums, and install it:

```
$ curl -LO https://github.com/Claw-King/tcpraid/releases/download/v0.2.0/tcpraid-0.2.0-linux-arm64
$ curl -LO https://github.com/Claw-King/tcpraid/releases/download/v0.2.0/tcpraid-0.2.0-SHA256SUMS
$ grep linux-arm64 tcpraid-0.2.0-SHA256SUMS | sha256sum -c
tcpraid-0.2.0-linux-arm64: OK
# install -m 0755 tcpraid-0.2.0-linux-arm64 /usr/local/bin/tcpraid

$ tcpraid version
tcpraid 0.2.0 (wire protocol 1)
```

Anything but `OK` on the line of your file means the download is not what was
released, so do not run it.

**Or build it** (Go 1.27 or newer):

```
go build -trimpath -o tcpraid ./cmd/tcpraid
```

or build all the release binaries with `deploy/build.sh 0.2.0`. The header of
that script shows how to do that without Go installed. Copy the one for your
machine to `/usr/local/bin/tcpraid`. A binary from the plain `go build`
reports `0.2.0-dev`, while the build script puts in the version it is given.

## On macOS and on Windows

The same program runs as a node on macOS and on Windows, and pairs with nodes
on any of the three systems. These two are new, and so far the program has
been run there by the automated tests of the repository and once by hand, when
a Mac and a Windows machine were paired with each other. See "Known limits" in
the README.

| System | File |
|---|---|
| macOS, Apple silicon | `tcpraid-0.2.0-macos-arm64` |
| macOS, Intel | `tcpraid-0.2.0-macos-amd64` |
| Windows, x64 | `tcpraid-0.2.0-windows-amd64.exe` |
| Windows, ARM | `tcpraid-0.2.0-windows-arm64.exe` |

**Check the download.** On macOS, in a terminal, in the directory with the
two files:

```
$ grep macos-arm64 tcpraid-0.2.0-SHA256SUMS | shasum -a 256 -c
tcpraid-0.2.0-macos-arm64: OK
$ chmod +x tcpraid-0.2.0-macos-arm64
$ ./tcpraid-0.2.0-macos-arm64 version
```

On Windows, in PowerShell, compare the two values by eye. They must be the
same, except that the case of the letters does not matter:

```
> (Get-FileHash .\tcpraid-0.2.0-windows-amd64.exe -Algorithm SHA256).Hash
> Select-String windows-amd64 .\tcpraid-0.2.0-SHA256SUMS
> .\tcpraid-0.2.0-windows-amd64.exe version
```

**The binaries are not signed.** The first time you start one, the system may
stop it and say that it cannot check where the program comes from. If you
have checked the SHA-256 sum, allow it the way the system's message or its
security settings offer. In the one run we know of, a binary copied to a Mac
with `scp` and started from a terminal was not stopped. A file downloaded
with a browser may be treated differently, and that was not tried. A node
that listens for other machines (`-listen`, or a rule's port) may also make
the system ask whether the program may accept incoming connections. macOS
did, with the buttons Deny and Allow. Allow it, or the other node cannot
connect.

**Run it** from a terminal. The commands of tcpraid and their flags are the
same as on Linux, with the name of the file you downloaded in place of
`tcpraid`. In PowerShell that is `.\tcpraid-0.2.0-windows-amd64.exe`. The
shell lines around the commands in this documentation are written for Linux,
and we have not tried them on these systems. What differs:

- **The data directory.** Without `-data`, a node keeps its data in a
  directory `tcpraid` in your user's configuration directory. On macOS that
  is `~/Library/Application Support/tcpraid`, and on Windows
  `%AppData%\tcpraid`.
  Every command that talks to the node (`status`, `pair`, `peers`, `rules`,
  `sessions`) looks there too when run as the same user.
- **Only on Linux:** `-transparent` and transparent rules, `-maxrss`, the
  `rss` figure of the `-stats` line (shown as `n/a` elsewhere), the container
  image, and the systemd unit. `tcpraid run` refuses `-transparent` and
  `-maxrss` at its start on the other systems.
- **Keeping it running.** The program runs for as long as its terminal does.
  Stop it with Ctrl+C before you close the terminal, because closing the
  window ends the program immediately, without a word to the other node. To
  have it start with the system, use what the system offers for that, such as
  `launchd` on macOS, or a scheduled task or a service wrapper on Windows. We
  ship no installer and no service definition for these systems.
- **A machine that sleeps stops its node for that time.** Transfers stand
  still while it sleeps, and the other node gives a session up when its
  connections to this one have been down for the 15 minutes of `-grace`. We
  have not tried a node through a sleep, long or short.

## Start a node

By hand, to try it:

```
nas-a# tcpraid run -listen 0.0.0.0:9851 -advertise nas-a.example.org:9851
... web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
... web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
... node 0.2.0 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
... transport window=128MB budget=256MB grace=15m0s hol=1s timeline=off maxrss=off
... web interface: open http://127.0.0.1:9850 in a browser on this machine
```

- `-listen` is where other nodes connect. Give it on the node that the other
  one can reach. The other node needs none. If neither site has a public
  address, `connecting.md` goes through the ways to get one node reachable.
- `-advertise` is the address the other node will use for it. It goes into
  the pairing codes this node makes.
- The data directory is `/var/lib/tcpraid` unless `-data` says otherwise. It
  holds the node's private key, so keep it, and back it up like one.
- The web page and the API are at `127.0.0.1:9850`, reachable from the host
  itself only. To open the page from another machine, add
  `-api ADDRESS:9850`. It is plain HTTP, so do that only on a network you
  trust.

With systemd, `deploy/tcpraid-node.service` is a unit for this. Copy it to
`/etc/systemd/system/` and adjust the command line in it. On a node that the
other one connects to, add `-advertise HOST:9851` there, because
`tcpraid pair create` puts that address into the code, and without one it
needs `-addr` each time. Then run `systemctl enable --now tcpraid-node`. The
generated password is in
`journalctl -u tcpraid-node`.

```
nas-a$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.2.0 (wire protocol 1)
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
Enter it on the other node with "tcpraid pair join", which then reads the code from standard input.
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

Fingerprints are shortened in the table here.

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
`nas-b`. It then asks `nas-b` to open the connections, as `concepts.md`
explains.

## On a router: transparent rules

A node on the router or gateway of a site can carry connections without the
applications knowing. The applications keep their normal addresses, and the
router redirects the connections to the node.

1. Start the node with a transparent listener:
   `tcpraid run -transparent 0.0.0.0:9800 ...`
2. Redirect the connections to it. tcpraid does not change the host's packet
   filter, so you add the rule yourself. With nftables, for connections that
   pass through the router from the LAN interface:

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
   by commas, for example `192.0.2.0/24:873,192.0.2.0/24:50000-50010`.

The first transparent rule that matches takes the connection, and a
redirected connection that no rule matches is reset. The connection arrives at the same
address at the peer's end, so that address must mean the same machine there.

For this document, locally made connections were redirected (the `output`
hook) to a node with a transparent rule, and 20 MiB went through it with equal
checksums at both ends. The `prerouting` form above, for connections passing
through a router, is the one in `examples/redirect.nft`. The first user of
version 0.1.0 ran it on a router, with a storage replication task as the
load.
