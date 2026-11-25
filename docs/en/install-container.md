# A node in a container

This is the way to run tcpraid on a NAS. You need one node at each of the two
sites. The steps were run with Docker on a Linux machine. No step was tried
on a real NAS (see "Where each platform takes it").

## What the container needs

| | |
|---|---|
| Image | `ghcr.io/claw-king/tcpraid:0.12.0` (you can also build it yourself with `deploy/Dockerfile`) |
| Data | a directory mounted at `/data`. It holds the node's private key, its peers and its rules. Keep it, back it up like a private key, never share it between two nodes |
| Port 9850 | the web page and the local API. Plain HTTP unless the node is started with `-api-tls` (`security.md`): publish it only on a network you trust |
| Port 9851 | where other nodes connect. Needed on the node that the other one can reach; harmless on the other |
| Ports 50000-50010 | for forwarding rules. A rule made in the web page must listen on one of the ports you publish, at the address `0.0.0.0` |
| Port 8446, UDP | optional: feedback from the other node over UDP. Off unless the node is started with `-udp-listen` |
| `TCPRAID_UI_PASSWORD` | optional: the password of the web page. Without it the node makes one and prints it once in its log |

`deploy/docker-compose.yml` has all of this, with comments. The range for
rules is only a default. To change it, set both sides of the mapping to the
same ports. The file also shows the alternative of sharing the host's network
instead of mapping ports. A rule can then listen on any free port of the host.

UDP feedback is off by default. To use it, add `-udp-listen 0.0.0.0:8446` to the
node's command line, publish port 8446 as UDP, and tell the other node the
address with `tcpraid peers set NAME -udp-dial HOST:8446`. The compose file has
both lines in it, commented out.

**With the host's network, say where the web page listens.** The image starts
the node with `-api 0.0.0.0:9850`, which is right behind a port mapping. On
the host's network it opens the web page and the API, plain HTTP, on every
address of the host, including one that faces another network, as on a router
or a gateway. Give the container its own command there, with the address the
page should be at, which is either the host itself only or one address of the
LAN:

```
    network_mode: host
    command: ["run", "-data", "/data", "-api", "127.0.0.1:9850", "-listen", "0.0.0.0:9851"]
```

With `127.0.0.1` the page opens from the host only. From another machine,
reach it through a tunnel such as `ssh -L 9850:127.0.0.1:9850 HOST`.

## Start

```
$ docker compose up -d
$ docker logs tcpraid
... web interface: no password was set, so one was made. It is shown this once: y7ygtkha76gshsnt
... web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
... node 0.12.0 name="nas-a" fingerprint=056f8829...e2e077d8 data=/data api=[::]:9850 listen=[::]:9851 listen-plain=off transparent=off udp-listen=off
... transport window=128MB budget=256MB grace=15m0s hol=1s timeline=off maxrss=off
... web interface: open http://127.0.0.1:9850 in a browser on this machine
... the web interface and the API at [::]:9850 are plain HTTP and reachable from other hosts. Use them on a network you trust, or put a reverse proxy with TLS in front.
```

Open `http://ADDRESS-OF-THE-HOST:9850/` and log in with that password.

**Name the node before you pair it.** At its first start a node takes the
container's host name and adds four random characters. With the compose file
as it comes that gives a name like `tcpraid-k3x9`, different on every machine.
Give each node a name you recognise in Settings > "Name of this node". This
page uses `nas-a` and `nas-b`. Do it before pairing, because each node records
the name its peer had at that moment, rules name the peer by it, and a later
rename does not reach the other node.

## Pair the two nodes

One node has to be reachable from the other on port 9851. Call that one
`nas-a`, reachable at `nas-a.example.org`, and the other `nas-b`. If neither
site has a public address, see `connecting.md` first.

In the web pages:

1. On `nas-a`, go to Peers > "Create pairing code". In "Address at which the
   other node reaches this one" enter `nas-a.example.org:9851`. A node in a
   container does not know its outside address, so it has to be told. Copy
   the code. It is shown once, works once and is valid for an hour.
2. On `nas-b`, go to Peers > "Enter pairing code", paste the code and click
   "Pair".
3. Both pages now list the other node, within a few seconds as online. On
   `nas-b`, the peer's Details have a "Check connection" button.

Or with commands. In the container the program is `/tcpraid` and the data
directory is `/data`:

```
nas-a$ docker exec tcpraid /tcpraid pair create -data /data -addr nas-a.example.org:9851
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node with "tcpraid pair join", which then reads the code from standard input.
It is shown this once and is a secret until it is used.
tcpraid-pair-1-iiph6pbe4iey...

nas-b$ docker exec -i tcpraid /tcpraid pair join -data /data
tcpraid-pair-1-iiph6pbe4iey...        <- pasted; then Enter and Ctrl-D
paired with nas-a (fingerprint 056f8829...e2e077d8)

nas-b$ docker exec tcpraid /tcpraid peers -data /data
NAME   STATE   FINGERPRINT           PAIRED                PATHS
nas-a  online  056f8829...e2e077d8   2026-01-15T10:00:00Z  nas-a.example.org:9851 (tls, data)

nas-a$ docker exec tcpraid /tcpraid peers -data /data
NAME   STATE   FINGERPRINT           PAIRED                PATHS
nas-b  online  36dc0793...0fe0ca7f   2026-01-15T10:00:00Z  none: this node cannot reach it
```

The fingerprints are 64 hex digits and are shortened here. Without `-addr`,
`pair create` in a container answers:

```
tcpraid pair create: no address for the other node to connect to. Name one in the request, or start the node with -advertise HOST:PORT.
```

"none: this node cannot reach it" on `nas-a` is normal. `nas-b` keeps a
connection open to `nas-a`, and rules on `nas-a` use it to ask `nas-b` to
connect, as "Which node connects" in `concepts.md` explains.

## Make a rule

On the node next to the application that starts the transfer, say `nas-b`,
go to Rules > "New rule":

- Name: anything, for example `rsync`.
- Listen address and port: `0.0.0.0:50000` (a port you published).
- Peer: `nas-a`.
- Destination address and port at the peer: the service at the other site, as
  `nas-a` reaches it, for example `192.0.2.10:873`. A host name works too, as
  in `nas.lan:873`. It is looked up by `nas-a`, for every connection.

Or:

```
nas-b$ docker exec tcpraid /tcpraid rules add rsync -data /data -peer nas-a -listen 0.0.0.0:50000 -dest 192.0.2.10:873
rule rsync added: listening
```

Now connect the application to `nas-b`'s address, port 50000, instead of
`192.0.2.10` port 873. The tutorials show this for particular applications.

To reach a service on the other NAS **itself** from its own container, use the
NAS's LAN address as the destination, not `127.0.0.1`, because inside a
container `127.0.0.1` is the container.

## Where each platform takes it

| Platform | How |
|---|---|
| TrueNAS (24.10 or later) | Apps > Discover Apps > Install via YAML, with `deploy/docker-compose.yml` |
| Synology | Container Manager > Project, with the compose file |
| QNAP | Container Station > Applications > Create, with the compose file; or the custom template `deploy/qnap/tcpraid-templates.json` |
| Unraid | the container template `deploy/unraid/tcpraid.xml` |
| any Linux with Docker | `docker compose up -d` |

`deploy/README.md` describes the files. None of these was tried on the real
product. The menu names come from the vendors' documentation, and the
templates were checked against the published formats only. On a NAS, give the
data directory an absolute path on a dataset or shared folder.

## Updating

Replace the image and start the container again on the same data directory.
The node keeps its identity, its peers and its rules. Sessions that were
running are reset, and the applications have to try again.
