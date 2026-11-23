# Tutorial: a Proxmox Backup Server sync job

A Proxmox Backup Server (PBS) sync job copies backups from a **remote** --
another PBS -- to a local datastore. By default it uses a single HTTPS
connection (HTTP/2 over TLS, port 8007) to the remote. This tutorial puts
that connection through tcpraid.

**What was tried and what was not.** PBS itself was **not run** for this
tutorial. What was run: a forwarding rule in front of a TLS server, with the
check that a client connecting through the rule sees the same certificate
fingerprint as a client connecting directly. The PBS commands below are taken
from the PBS documentation (links at the end) and were not executed.

**Look at PBS's own option first.** According to its documentation, a sync
job has a `worker-threads` option that synchronizes several backup groups in
parallel, each on its own connection. If your PBS version has it and you sync
many groups, that may be all you need. tcpraid is for what remains one
connection: a large backup group, or a version without that option.

## The setup

| | Site A | Site B |
|---|---|---|
| has | the PBS that holds the backups, 192.0.2.10 port 8007 | the PBS that pulls them |
| tcpraid node | `node-a` | `node-b` |

The two nodes are paired (`install-container.md` or `install-binary.md`).

## Step 1: a rule on node-b

Rules > "New rule" in the web page of `node-b`: listen address and port
`0.0.0.0:18007`, peer `node-a`, destination `192.0.2.10:8007`. Or:

```
node-b$ tcpraid rules add pbs -peer node-a -listen 0.0.0.0:18007 -dest 192.0.2.10:8007
rule pbs added: listening
```

With `node-b` in a container, use a port the container publishes, for example
50000 instead of 18007, here and in the commands below.

## Step 2: point the remote at the rule

On the pulling PBS, the remote's host and port become those of the rule. The
fingerprint stays the one of the PBS at site A: tcpraid passes the TLS
connection through untouched.

```
proxmox-backup-manager remote create pbs-a --host 198.51.100.5 --port 18007 \
    --auth-id sync@pam --password 'SECRET' --fingerprint 64:d3:ff:...:ab:fe
```

Here `198.51.100.5` is `node-b`. For an existing remote, the same options are
on `proxmox-backup-manager remote update`. The sync job itself does not
change.

The check that was run, with a test server in the place of the PBS: the
fingerprint a client sees directly and through the rule.

```
direct:  sha256 Fingerprint=47:3E:78:28:03:9E:08:89:...:CF:15:A3:5D
by rule: sha256 Fingerprint=47:3E:78:28:03:9E:08:89:...:CF:15:A3:5D
```

## What is protected

PBS encrypts its connection itself (TLS). Between the two nodes that
connection is carried inside tcpraid's own TLS as well. That is encryption
twice on that stretch; it costs some CPU and is the safe default.

## Sources

- PBS documentation, Managing Remotes and sync jobs (the `remote create`
  example; `worker-threads`: "a single HTTP/2 connection to the source is
  otherwise bottlenecked by head-of-line blocking"),
  `https://pbs.proxmox.com/docs/managing-remotes.html`
- PBS documentation, command syntax of `proxmox-backup-manager remote create`
  (`--host`, `--auth-id`, `--password`, `--fingerprint`, `--port`),
  `https://pbs.proxmox.com/docs/command-syntax.html`
