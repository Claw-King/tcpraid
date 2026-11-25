# Tutorial: the file share of the other site

A file server at site A, and a computer at site B that mounts its share as if
it were in the same building. This document is about the speed of that, which
depends less on tcpraid than on how the computer asks for the data.

**What was tried.** One user mounted the file share of a NAS at one site from
a computer at another, over a long path, through a transparent rule on the
router, and played video from it with a player built on mpv. **Not
measured:** how the same share does over the same path without tcpraid.

## The setup

| | Site A | Site B |
|---|---|---|
| has | the file server, at 192.0.2.10 | the computer that mounts the share |
| tcpraid node | `nas-a` | `gw-b`, on the router of site B |

The two nodes are paired (`install-container.md` or `install-binary.md`). The
file server listens on port 445 in this example.

## Step 1: a rule on the node of site B

With a node on the router of site B that is started with `-transparent` (and
`-redirect`, or a redirect rule of your own, `reference-files.md`), a
transparent rule takes the connections to the file server, and the computer
mounts the share at the same address as it does at site A:

```
gw-b$ tcpraid rules add share -peer nas-a -match 192.0.2.10/32:445 -coalesce 0s
rule share added: listening, with coalesce=0s
```

Without a node on the router, a node on another machine at site B, paired
with `nas-a` in the same way, does the same with a fixed rule, and the
computer mounts the share at that machine's address instead. Port 445 has to
be free on that machine, so it cannot be one that serves files itself:

```
node-b$ tcpraid rules add share -peer nas-a -listen 0.0.0.0:445 -dest 192.0.2.10:445 -coalesce 0s
```

`-coalesce 0s` makes the node send each piece the application gives it
without waiting for more (`reference-files.md`). An application that sends a
small request and waits for the answer needs that. The data of the files come
from site A, so the setting has to act at `nas-a` as well. A rule's setting
does that when both nodes run version 0.12.0 or later. With an older node at
site A, set it there for the peer instead:
`tcpraid peers set gw-b -coalesce 0s` on `nas-a`.

## Step 2: what decides the speed

Reading a file over a file share is a series of requests and answers. In
SMB 2, for example, the client sends a READ request that names where in the
file to read and how many bytes, and the server sends the data back in a
READ response (`[MS-SMB2]`, sections 2.2.19 and 2.2.20). Each answer takes at
least one round trip over the path. So the speed is about how much the
computer asks for at a time and how many requests it keeps under way,
divided by the round trip. Over a long distance that can be far below what
the path carries. Apart from `-coalesce 0s` in step 1, no setting of tcpraid
changes it. The user above saw this with one computer and one player.

`tcpraid sessions` at the node of site A, which sends the data, then shows
the state "ok, waiting for the application", and the Sessions page of that
node says "waiting for the application" (`troubleshooting.md`, "It works but
it is not faster").

To compare, copy one large file from the share to a local disk and time it.
If the copy is clearly faster than the program you care about, that program
is the limit, and the place to change something is the program.

## Step 3: playing video

A video player that reads a little at a time, and asks for the next part
only when it needs it, stalls over a long distance although the path has
room. What helps is to let the player read more at a time and further ahead.

The mpv player, for example, has these options (from its manual):

| Option | What the manual says |
|---|---|
| `stream-buffer-size` | "Size of the low level stream byte buffer (default: 128KB)." It is used as a buffer between the demuxer and the low-level reading, from a socket for example |
| `demuxer-max-bytes` | "This controls how much the demuxer is allowed to buffer ahead." |
| `demuxer-readahead-secs` | "If --demuxer-thread is enabled, this controls how much the demuxer should buffer ahead in seconds (default: 1)." The manual adds: "--cache-secs will override this. Technically, the maximum of both options is used." |

In mpv's configuration files an option is written without the two
dashes, as `option=value`. Sizes take suffixes such as `KiB` and `MiB`. The
values below are an example for a player that reads over a long path, not
values that are best anywhere:

```
stream-buffer-size=16MiB
demuxer-max-bytes=1024MiB
demuxer-readahead-secs=120
```

## Notes

- A file share sees every client as coming from the node at site A. A
  restriction by client address on the file server can no longer tell the
  clients at site B apart.
- The connection between the two nodes is encrypted by default
  (`security.md`).
