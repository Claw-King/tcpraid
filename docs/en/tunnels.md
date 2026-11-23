# Using tcpraid with a tunnel, an overlay network or a proxy

A path to a peer is just an address that TCP connections can be opened to.
That address does not have to be the peer's own: it can be the near end of
anything that carries TCP to the peer's listener. This document says how to
configure that.

## The rule

- **The path's address** is the address the dialing node connects to in order
  to reach the peer's listener: the address of a port forward, of the tunnel's local end, of the
  peer inside an overlay network, or -- for a SOCKS5 proxy -- the address the
  proxy should connect to.
- **The path's tier** says whether tcpraid adds its own TLS on that path:
  `tls` (the default) does, `off` does not.

Set both on the node that dials, in the web page (Peers > Details > the
path > Edit) or with `tcpraid peers path set`.

## A port forward or a tunnel with a fixed far end

Something listens near this node and delivers to the peer's port 9851 (an SSH
port forward, a relay, a forwarded port on a router). Point the path at the
near end:

```
nas-b$ tcpraid peers path set nas-a default -addr 127.0.0.1:19851
```

The tier stays `tls`: the two nodes still authenticate each other and encrypt
end to end, inside whatever carries the connection.

## An overlay network or a VPN

Both nodes have an address in the overlay. Use the peer's overlay address as
the path's address, and give that address when you make the pairing code
(the address at which the other node reaches this one). Nothing else changes.

## A path that is already encrypted: the `off` tier

If the tunnel encrypts from node to node, tcpraid's own TLS is a second layer.
To leave it out:

1. Start the listening node with a plain listener as well:
   `tcpraid run ... -listen-plain 0.0.0.0:9852`. Make sure that port can be
   reached **only** through the tunnel.
2. On the dialing node, switch the path over:

   ```
   nas-b$ tcpraid peers path set nas-a default -security off -addr 127.0.0.1:19852
   path default of nas-a changed
   PATH     TYPE  ADDRESS          PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS
   default  tcp   127.0.0.1:19852  -      off       -       -    data   8

   nas-b$ tcpraid peers check nas-a
   default  127.0.0.1:19852 (off): ok
   ```

In the `off` tier the data is not encrypted by tcpraid, and the only proof of
who connects is a short authenticated opening that could be replayed within
ten minutes by someone who can read the path (`security.md`). Use it only
where the tunnel covers the whole way between the two nodes.

Pairing itself always needs the TLS listener (`-listen`); a node can run both
listeners. Rules on the node that does not dial work over an `off` path too:
it was run with one node listening on the plain listener only, and a rule on
that node carried 60 MiB to the other with equal checksums.

## Through a SOCKS5 proxy

A path can go through a SOCKS5 proxy near the dialing node. The path's address
is then what the proxy connects to:

```
nas-b$ tcpraid peers path set nas-a side -addr 192.0.2.10:9851 -type socks5 \
           -proxy socks5://user:pass@127.0.0.1:1080 -roles repair,control -subs 2
```

A proxy's password given on the command line shows in the process list; the
web page takes it too, and never shows it again. Paths through a proxy were
not part of the runs for this documentation.

## Several paths

A peer can have several paths, over different routes, with different roles
(`concepts.md`). A common shape: the main route with the role `data`, and a
second, independent route with `repair` and `control`.

```
nas-b$ tcpraid peers show nas-a
name         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
paired       2026-01-15T10:00:00Z
addresses    nas-a.example.org:9851
state        online, runs 0.1.2, last heard 2026-01-15T10:00:05Z

PATH     TYPE    ADDRESS                 PROXY                    SECURITY  VERIFY  PIN  ROLES           SUBS
default  tcp     nas-a.example.org:9851  -                        tls       -       -    data            8
side     socks5  192.0.2.10:9851         socks5://127.0.0.1:1080  tls       -       -    repair,control  2
```

`tcpraid peers path remove NAME PATH` removes a path. `tcpraid peers check
NAME` tries each path and says which of them fail.

## Your own certificate: the `owncert` tier

Where the listening node should show a certificate of yours instead of its
identity certificate -- for example one issued by an authority, on a host name
-- start it with `-cert FILE -key FILE` (next to `-listen`) and set the path on
the dialing node:

```
# checked by the fingerprint of the certificate's key (the listening node logs it at start)
tcpraid peers path set nas-a default -security owncert -pin FINGERPRINT

# or checked by authority and host name, as a web client would
tcpraid peers path set nas-a default -security owncert -verify ca -addr nas-a.example.org:9851
```

The dialing node still shows its identity certificate, and the opening still
proves the pairing. This tier is covered by the program's tests; it was not
part of the runs for this documentation.
