# Tutorial: PostgreSQL streaming replication

A PostgreSQL standby gets its data from the primary over ordinary PostgreSQL
connections: `pg_basebackup` copies the whole cluster over one connection (and
a second one for the write-ahead log with `-X stream`), and afterwards the
standby streams the log over one connection. This tutorial puts those
connections through tcpraid. It matters most for the base backup of a large
database over a long path.

**What was tried.** The commands below, in a container, with PostgreSQL 17:
a primary behind one node, `pg_basebackup` through a rule on the other node
(137 MB), the standby started from it, rows inserted on the primary and found
on the standby. Then the node at the primary's site was stopped for five
seconds and started again: the standby reconnected by itself and caught up.
**Not tried:** a large database, a long path, TLS between PostgreSQL client
and server, authentication other than `trust` on the loopback address.

## The setup

| | Site A | Site B |
|---|---|---|
| has | the primary, at 192.0.2.10 port 5432 | the standby |
| tcpraid node | `nas-a` | `nas-b` |

The two nodes are paired. The primary accepts replication connections from
the address of `nas-a` (to the primary, the connections come from the node at
its own site): a role with the `REPLICATION` attribute and a matching
`replication` line in `pg_hba.conf`, as for any standby.

```
site-a$ psql -c "create role repl replication login"
# pg_hba.conf on the primary; 192.0.2.5 is nas-a
host replication repl 192.0.2.5/32 METHOD
```

`METHOD` is the authentication method you use for your standbys. (The run for
this tutorial had everything on one machine and used `trust` for the loopback
address, which is for tests only.)

## Step 1: a rule on nas-b

Rules > "New rule" in the web page of `nas-b`: listen address and port
`127.0.0.1:15432` if the standby is on the same host as the node, otherwise
an address the standby can reach; peer `nas-a`; destination
`192.0.2.10:5432`. Or:

```
nas-b$ tcpraid rules add pg -peer nas-a -listen 127.0.0.1:15432 -dest 192.0.2.10:5432
rule pg added: listening
```

If `nas-b` runs in a container, `127.0.0.1` inside it is the container's own:
listen on `0.0.0.0` with a port the container publishes (for example 50000),
and point the standby at the host's address and that port. And if `nas-a` runs
in a container on the primary's own host, the primary sees the connection
coming from the container's address on the host, not from 192.0.2.5: the
`pg_hba.conf` line has to name that address.

## Step 2: the base backup through the rule

On the standby's host, point `pg_basebackup` at the rule instead of at the
primary:

```
site-b$ pg_basebackup -h 127.0.0.1 -p 15432 -U repl -D /var/lib/postgresql/standby -R -X stream
```

`-R` writes the configuration for replication (`standby.signal` and a
`primary_conninfo` line), so the standby will keep using the rule's address:

```
primary_conninfo = 'user=repl ... host=127.0.0.1 port=15432 ...'
```

## Step 3: start the standby and check

Start PostgreSQL on the new data directory. On the primary:

```
site-a$ psql -c "select client_addr, state, sync_state from pg_stat_replication"
 client_addr |   state   | sync_state
-------------+-----------+------------
 192.0.2.5   | streaming | async
```

The client address is that of `nas-a`. On the node, the standby's connection
is a session that stays for as long as replication runs:

```
nas-b$ tcpraid rules
NAME  TAKES            PEER   DESTINATION      SESSIONS  STATE
pg    127.0.0.1:15432  nas-a  192.0.2.10:5432  1         listening
```

## What to expect

- The log stream of a quiet database is small, and tcpraid adds nothing to
  it. The gain, if the path is one where parallel connections help, is in the
  base backup and in catching up after a long break.
- If the path fails, both nodes hold the connections for up to 15 minutes and
  continue. If a node restarts, the standby's connection is reset and
  PostgreSQL connects again, as it does after any broken connection.
- Between the two nodes the stream is inside TLS 1.3. Between PostgreSQL and
  its node at each site it is whatever PostgreSQL itself does on that
  connection.

## Sources

- PostgreSQL documentation: `pg_basebackup` ("The backup is made over a
  regular PostgreSQL connection that uses the replication protocol"; `-X
  stream` "will open a second connection to the server"),
  `https://www.postgresql.org/docs/current/app-pgbasebackup.html`; log-shipping
  standby servers, `https://www.postgresql.org/docs/current/warm-standby.html`
- `pg_basebackup --help` of PostgreSQL 17.12: "-R, --write-recovery-conf  write
  configuration for replication".
