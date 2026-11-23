# Tutorial: rsync to an rsync daemon

rsync can talk to a remote system in two ways: through a remote shell such as
ssh, or directly to an **rsync daemon** over TCP, normally on port 873. A run
of rsync talks to the daemon over one TCP connection; its manual has no option
to use several. This tutorial puts that connection through tcpraid.

**What was tried.** Exactly the commands below, in a container: a real rsync
daemon behind one node, a real rsync client through a rule on the other node,
43 MB in three files; `diff -r` found no difference afterwards. **Not tried:**
any NAS. The notes on NAS products at the end come from their documentation.

## The setup

| | Site A | Site B |
|---|---|---|
| has | the rsync daemon, at 192.0.2.10 port 873 | the data to send |
| tcpraid node | `nas-a` | `nas-b` |

The two nodes are paired (`install-container.md` or `install-binary.md`).

## Step 1: the daemon at site A

If you already have an rsync daemon (an "rsync server" on a NAS), skip this.
A minimal one, for testing, with a module called `backup`:

```
# /etc/rsyncd.conf
use chroot = no
[backup]
    path = /srv/backup
    read only = no
    uid = root
    gid = root
```

```
site-a# rsync --daemon --config=/etc/rsyncd.conf --port=873
```

This daemon accepts anyone who can reach it. For real use, restrict who may
use the module, as the rsync documentation (`rsyncd.conf`) describes. Mind one
thing: through a rule, the daemon sees every client as coming from `nas-a`, so
a restriction by client address (`hosts allow`) can no longer tell clients
apart -- whoever can reach the rule's port on `nas-b` arrives as `nas-a`.
Restrict by user and password instead (`auth users` and `secrets file`), and
make the rule listen only where the clients you mean can reach it.

## Step 2: a rule on nas-b

In the web page of `nas-b`: Rules > "New rule", listen address and port
`0.0.0.0:8873`, peer `nas-a`, destination `192.0.2.10:873`. Or:

```
nas-b$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:8873 -dest 192.0.2.10:873
rule rsync added: listening
```

With `nas-b` in a container, use a port the container publishes, for example
50000 instead of 8873.

## Step 3: rsync to the rule

Give rsync the address of `nas-b` and the rule's port in place of the
daemon's:

```
site-b$ rsync -a --stats /data/ rsync://nas-b.example.org:8873/backup/

Number of files: 5 (reg: 3, dir: 2)
Number of created files: 4 (reg: 3, dir: 1)
Number of regular files transferred: 3
Total file size: 43,000,006 bytes
Total transferred file size: 43,000,006 bytes
...
sent 43,010,778 bytes  received 85 bytes  86,021,726.00 bytes/sec
total size is 43,000,006  speedup is 1.00
```

While it runs, the rule shows a session:

```
nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:8873  nas-a  192.0.2.10:873  1         listening
```

The other direction, pulling from the daemon, is the same command with source
and destination swapped. A rule carries the connection whichever way the
data flows.

## What is protected

The rsync daemon protocol itself is not encrypted. Between the two nodes the
connection is inside TLS 1.3; between rsync and its node at each site it is
plain, on the local network.

## rsync on NAS products

rsync to a daemon is the transfer that is known to fit tcpraid on NAS products
other than TrueNAS; whether their other replication features open one
connection or several is not documented by the vendors, and was not tested.

- **Synology**: the documentation lists port 873 for rsync without transfer
  encryption (Shared Folder Sync, Hyper Backup to an rsync server).
- **QNAP**: Hybrid Backup Sync has rsync jobs to a remote server, with a
  field for the port.
- **TrueNAS**: Rsync Tasks have the mode "Module", which talks to a daemon.
  Its documentation page does not show a field for the daemon's port.

Where the product lets you set the server's address and port, put in the
node's address and the rule's port. Where it only lets you set the address,
let the rule listen on port 873 -- which works only if nothing else on the
node's host uses that port, so not on a NAS that runs its own rsync server.
None of this was tried on the products.

## Sources

- rsync manual page: the two ways to contact a remote system; "Connections to
  an rsync daemon typically use TCP port 873".
  `https://raw.githubusercontent.com/RsyncProject/rsync/master/rsync.1.md`
- Synology: service port list,
  `https://kb.synology.com/en-uk/DSM/tutorial/What_network_ports_are_used_by_Synology_services`
- QNAP Hybrid Backup Sync: creating a storage space on a remote server,
  `https://docs.qnap.com/application/hybrid-backup-sync/3v21.x/en-us/creating-a-storage-space-on-a-remote-server-9B6F1BA0.html`
- TrueNAS: Rsync Tasks screens,
  `https://www.truenas.com/docs/scale/dataprotection/rsync/rsynctasksscreens/`
