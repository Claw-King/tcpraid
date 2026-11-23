# Tutorial: ZFS replication between two TrueNAS systems

A TrueNAS replication task sends its data over a single TCP connection. This
tutorial puts that connection through tcpraid, so that it travels over several
connections between the two sites.

**What was tried and what was not.** The tcpraid side of this tutorial -- a
rule over a range of ports, data sent through each port, checksums compared at
both ends -- was run in containers, with a plain TCP sender and receiver in the
place of the two NAS. The TrueNAS steps were written from the TrueNAS
documentation and the source of its replication engine (links at the end).
**None of these steps was tried on a real TrueNAS with this version** (the
last section, for nodes on routers, describes another way in that was). If a screen
looks different on your system, trust the screen.

## Before you start

- Check that it can help: between the two sites, measure with `iperf3` (a
  network speed test tool: `iperf3 -s` on one side, then on the other
  `iperf3 -c HOST` for one stream and `iperf3 -c HOST -P 8` for eight). If
  eight together are not clearly faster than one, tcpraid will not make
  replication faster.
- A tcpraid node at each site, paired (`install-container.md` or
  `install-binary.md`). The nodes can run on the two TrueNAS systems
  themselves, as apps, or on another machine at each site.

The names used here:

| | Site A (source) | Site B (destination) |
|---|---|---|
| TrueNAS | `truenas-a`, 192.0.2.10 | `truenas-b`, 198.51.100.20 |
| tcpraid node | `node-a`, 192.0.2.5 (or 192.0.2.10 if it runs on the NAS) | `node-b` |

The task is a **push** from `truenas-a` to `truenas-b`, created on
`truenas-a`.

## How TrueNAS moves the data

A replication task has a **Transport**. With `SSH+NETCAT`, TrueNAS uses its
SSH connection to the other system only to set things up, and sends the data
on a separate, unencrypted TCP connection:

- One of the two systems is the **Netcat Active Side**: in the words of the
  TrueNAS documentation, "the system that opens ports". It listens on the
  first free port between **Min Port** and **Max Port**.
- The other system connects to the **Netcat Active Side Connect Address**, at
  the port the active side picked.

That data connection is the one to carry. We let `truenas-b` listen, and make
`truenas-a` connect to `node-a` instead of to `truenas-b`; `node-a` carries
the connection to `node-b`, which connects to `truenas-b`.

```
truenas-a --> node-a:50000-50010 ====== tcpraid ======> node-b --> truenas-b:50000-50010
 (connects)    (rule listens)                                       (netcat listens)
```

Because `truenas-a` connects to the same port number that `truenas-b` picked,
the rule maps a range of ports to the same range, port by port.

## Step 1: a rule on node-a for the range of ports

Pick a range that is free on `truenas-b`, for example 50000 to 50010.
TrueNAS takes the first free port of the range for a transfer, so a small
range is enough.

In the web page of `node-a`: Rules > "New rule".

- Name: `zfs`
- Listen address and port: `0.0.0.0:50000-50010`
- Peer: `node-b`
- Destination IP and port at the peer: `198.51.100.20:50000-50010`

Or with the command (for a node in a container, the command is
`docker exec tcpraid /tcpraid ... -data /data`, as in `install-container.md`):

```
node-a$ tcpraid rules add zfs -peer node-b -listen 0.0.0.0:50000-50010 -dest 198.51.100.20:50000-50010
rule zfs added: listening

node-a$ tcpraid rules
NAME  TAKES                PEER    DESTINATION                SESSIONS  STATE
zfs   0.0.0.0:50000-50010  node-b  198.51.100.20:50000-50010  0         listening
```

Two things to watch when the nodes run as containers on the NAS:

- On `truenas-a`, the container of `node-a` has to publish the ports of the
  range (the compose file in `deploy/` publishes 50000-50010).
- On `truenas-b`, the ports of the range must be **free on the NAS itself**,
  because TrueNAS is going to listen on one of them. If the container of
  `node-b` publishes the same range, remove that mapping there (`node-b` has
  no rule that needs it), or use another range for replication.

The destination is the LAN address of `truenas-b`, also when `node-b` runs on
`truenas-b`: inside a container, `127.0.0.1` is the container.

## Step 2: the SSH connection

TrueNAS still needs its SSH connection from `truenas-a` to `truenas-b`
(Credentials > Backup Credentials > SSH Connections). It carries little
traffic. Two cases:

- `truenas-a` can reach `truenas-b`'s SSH port already (a VPN, a port
  forward): leave the SSH connection as it is.
- It cannot: add a second rule on `node-a`, for example
  `-listen 0.0.0.0:50022 -dest 198.51.100.20:22`, and in the SSH connection
  (setup method Manual) set **Host** to the address of `node-a` and **Port**
  to 50022. tcpraid passes the bytes through untouched, so the host key that
  `truenas-a` sees is still the one of `truenas-b`. As with any manual SSH
  connection, TrueNAS also needs a private key whose public key `truenas-b`
  accepts. If `node-a` runs in a container, port 50022 has to be published as
  well: add `- "50022:50022"` to the `ports:` of its compose file (the range
  it publishes by default is taken by the `zfs` rule), or let the container
  share the host's network.

## Step 3: the replication task

On `truenas-a`: Data Protection > Replication Tasks > Add opens the wizard;
**Advanced Replication Creation** in it opens the Add Replication Task
screen, which is where the TrueNAS documentation describes the `SSH+NETCAT`
settings. Set the source, destination, snapshots and schedule as you would
without tcpraid. The settings that matter here:

| Setting | Value |
|---|---|
| Direction | Push |
| Transport | SSH+NETCAT |
| SSH Connection | the connection to `truenas-b` |
| Netcat Active Side | Remote (so that `truenas-b` listens) |
| Netcat Active Side Listen Address | leave it empty |
| Netcat Active Side Min Port | 50000 |
| Netcat Active Side Max Port | 50010 |
| Netcat Active Side Connect Address | the address of `node-a`: `192.0.2.5` (`192.0.2.10` if `node-a` runs on `truenas-a`) |

Without a connect address, TrueNAS would connect to the host of the SSH
connection, that is to `truenas-b` directly, and tcpraid would not be in the
path.

Leave the listen address empty: TrueNAS then listens on every address of
`truenas-b`. In the source of the replication engine as read for this
tutorial, an IPv4 address in that field, `0.0.0.0` included, makes the
listening helper fail when it starts, although the documentation page names
`0.0.0.0` as the default.

This was later seen on a TrueNAS of version 25.10: with an IPv4 address in
the field, the task fails and the web page shows only

```
Unknown SSH+NETCAT transport error: 'Traceback (most recent call last):\n'
```

which does not say why. If you see that message, look at this field first.
On that version the help text of the field in the web page (seen in the
Chinese interface) also says that the default is `0.0.0.0`; do not go by it.
Leave the field empty, or enter `::`.

## Step 4: run it and look

Run the task. While it runs, the web page of `node-a` shows a session under
Sessions, with its rule (`zfs`), its speed and its number of connections, and
`tcpraid rules` shows `1` under SESSIONS. On `node-b` the same session appears
as coming from the peer.

If the task fails at once, look at the rule's status on `node-a` (Rules page,
or `tcpraid rules`): it says why a connection could not be carried.
`troubleshooting.md` lists the usual causes.

If the path between the sites fails during a replication, both nodes hold the
connection for up to 15 minutes and continue when it is back. If a node is
restarted, the connection is reset, and TrueNAS sees what it would see with
any connection that broke.

## What travels unprotected

TrueNAS documents the `SSH+NETCAT` data stream as unencrypted, for trusted
networks. With this setup:

- between `node-a` and `node-b` the stream is inside TLS 1.3 (unless you set
  the path's tier to `off`);
- between `truenas-a` and `node-a`, and between `node-b` and `truenas-b`, it
  is unencrypted on the local network of each site. With the nodes on the NAS
  themselves, that stretch does not leave the machine.

## The other direction: a pull task

A pull task is created on the destination, `truenas-b`, and fetches from
`truenas-a`. The data still flows from A to B, and `truenas-a` is still the
side that should connect. Reasoning from the same documentation (not run):
set **Netcat Active Side** to Local, so that `truenas-b` listens, and
**Netcat Active Side Connect Address** to the address of `node-a` as
`truenas-a` reaches it; the rule on `node-a` stays as in step 1.

## With the nodes on the routers: an existing task, unchanged

If the two nodes run on the routers or gateways of the two sites, and
`truenas-a` already reaches `truenas-b` at its own address through them, a
replication task with the `SSH+NETCAT` transport can be carried as it is:
nothing changes in TrueNAS, no connect address, no forwarding rule. The
router redirects the task's connections to its node, and a transparent rule
names the peer (`install-binary.md`, "On a router: transparent rules").

On the router of the site whose TrueNAS opens the connections -- here
`gw-a`, because `truenas-b` is the Netcat Active Side and listens:

1. Start the node with `-transparent 0.0.0.0:9800`.
2. Redirect to it what `truenas-a` sends to `truenas-b` on the SSH port and
   on the task's range of netcat ports. With nftables:

   ```
   table ip tcpraid {
     chain pre {
       type nat hook prerouting priority dstnat; policy accept;
       iifname "lan0" ip daddr 198.51.100.20 tcp dport { 22, 50000-50010 } redirect to :9800
     }
   }
   ```

3. Make the transparent rule:

   ```
   gw-a$ tcpraid rules add zfs -peer gw-b -match 198.51.100.20/32:22,198.51.100.20/32:50000-50010
   rule zfs added: listening
   ```

Both connections of the task, SSH and data, now travel through tcpraid, and
`gw-b` connects to `truenas-b` at the same address and port. On `gw-b` the
allow list of the peer can be narrowed to the same destinations
(`tcpraid peers set gw-a -allow ...`). To go back, remove the redirect.

The first user of version 0.1.0 carried an existing replication task this
way, between two routers. Steps 1 to 4 above, with a forwarding rule and a
connect address, are still as described at the top: not tried on a real
TrueNAS.

## Sources

- TrueNAS documentation, Replication Task screens: the Transport options and
  the `SSH+NETCAT` settings quoted above.
  `https://www.truenas.com/docs/scale/dataprotection/replication/replicationscreens/`
- TrueNAS documentation, SSH Connections screens (Host and Port of a manual
  connection).
  `https://www.truenas.com/docs/scale/credentials/backupcredentials/sshcredentialscreens`
- zettarepl, the replication engine of TrueNAS: `ssh_netcat.py` (the passive
  side connects to the connect address at the port the active side picked;
  without a connect address and with the active side Remote it uses the host
  of the SSH connection; without a listen address it listens on `::`, every
  address) and `ssh_netcat_helper.py` (one listener, one accepted connection;
  it asks for a socket that takes IPv4 and IPv6, which an IPv4 listen address
  cannot give). `https://github.com/truenas/zettarepl`
