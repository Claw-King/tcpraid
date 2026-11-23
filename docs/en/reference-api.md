# The local HTTP API

A node serves an HTTP API at the address given with `-api` (default
`127.0.0.1:9850`). The commands and the web page are clients of it; anything
they do, another program on the host can do the same way.

The examples are real answers of a node of this version, with addresses,
times and long values changed for print. JSON is shown on several lines here;
the node sends it on one.

## Access

Every path but `/healthz` (and the files of the web page) needs the access
token of the node, which is in `api.token` in the data directory:

```
$ curl -H "Authorization: Bearer $(cat /var/lib/tcpraid/api.token)" http://127.0.0.1:9850/api/v1/status
```

Without it, or with a wrong one, the answer is `401` whether the path exists
or not:

```
HTTP/1.1 401 Unauthorized
Www-Authenticate: Bearer

{"error":"missing or wrong access token"}
```

A browser that is logged in to the web page is authorized by its cookie
instead (see "Login" at the end). The API is plain HTTP.

Errors are JSON, `{"error": "..."}`: `400` for a request that cannot be
right, `404` for a path or a name that does not exist, `405` for a method the
path does not have, `502` for a pairing that the other node did not complete
and `504` for one that ran out of time.

## Node

### GET /healthz

`200` and `ok`. Needs no token.

### GET /api/v1/status

```
{"version":"0.1.1","wire_version":1,"started":"2026-01-15T10:00:00.634920968Z","uptime_seconds":11,
 "node":{"name":"nas-b","fingerprint":"cb5a2afa6a827183541b967ed101aba925e7b9289447e21cdf5327599e688e83"},
 "sessions":2,"peers":1,"rules":2}
```

### PUT /api/v1/node

Renames the node. Peers keep the name the node had when they were paired with
it.

```
$ curl -X PUT ... -d '{"name":"nas-a"}' http://127.0.0.1:9850/api/v1/node
{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86"}
```

### GET /api/v1/sessions

The connections being carried. `role` is `client` on the node that took the
application's connection (that node also names the `rule`), `server` on the
node at the other end.

```
[{"id":"4fce0d73c066738a655b88c7741c2405","destination":"192.0.2.10:7000","role":"client",
  "started":"2026-01-15T10:00:31.329910062Z","tx_bytes":314572800,"rx_bytes":0,"unacked_bytes":0,
  "resent_bytes":0,"subs":8,"peer":"nas-a","rule":"rsync"}]
```

`tx_bytes` and `rx_bytes` are stream bytes sent and received; `unacked_bytes`
is what was read from the application and the other end has not yet reported
received, whether it has been sent already or still waits to be sent;
`resent_bytes` counts the bytes queued for sending again; `subs` is the
number of live sub-connections.

### GET /metrics

Counters in the Prometheus text format, prefixed `tcpraid_`.

## Pairing codes

### POST /api/v1/pairing-codes

Body `{"addresses": ["host:port", ...], "ttl_seconds": N}`, both optional
(default: the node's `-advertise` addresses, one hour). The answer holds the
code, this once.

```
$ curl -X POST ... -d '{"ttl_seconds":600}' http://127.0.0.1:9850/api/v1/pairing-codes
HTTP/1.1 201 Created
Cache-Control: no-store

{"id":"cb9c5e95","expires":"2026-01-15T10:10:30Z","used":false,"code":"tcpraid-pair-1-zoof5fkv3d4x..."}
```

### GET /api/v1/pairing-codes

The codes that have not expired, without the codes themselves.

```
[{"id":"a101905f","expires":"2026-01-15T11:00:20Z","used":true},
 {"id":"cb9c5e95","expires":"2026-01-15T10:10:30Z","used":false}]
```

### DELETE /api/v1/pairing-codes/{id}

Revokes a code. `204`.

## Peers

### POST /api/v1/peers

Pairs with the node that made a code. Body `{"code": "...", "address":
"host:port"}`; the address is optional and replaces the addresses in the
code. The answer is the new peer, as below.

### GET /api/v1/peers, GET /api/v1/peers/{name}

```
[{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86",
  "addresses":["nas-a.example.org:9851"],"paired_at":"2026-01-15T10:00:20Z",
  "paths":[{"name":"default","type":"tcp","addr":"nas-a.example.org:9851","security":"tls","roles":["data"],"subs":8}],
  "allow":"","udp_dial":"","online":true,"version":"0.1.1","last_message":"2026-01-15T10:00:26.938712846Z"}]
```

`online`: a control connection with the peer is up. `version` and
`last_message` appear once the peer has been heard from. A peer that joined
this node has no `addresses` and no `paths` here. The pair secret is never in
an answer, and a proxy is shown without its user and password.

### PUT /api/v1/peers/{name}

Changes a peer's settings. Body `{"paths": [...], "allow": "...", "udp_dial":
"..."}`, each optional, at least one. `paths` replaces all paths, each in the
form the `GET` shows; a path sent back with its proxy as shown keeps the user
and password it has. Everything is checked first; if something cannot be
stored, nothing changes. The answer is the peer.

```
$ curl -X PUT ... -d '{"allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":""}' http://127.0.0.1:9850/api/v1/peers/nas-b
{"name":"nas-b","fingerprint":"cb5a2afa...9e688e83","addresses":[],"paired_at":"2026-01-15T10:00:20Z","paths":[],
 "allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":"","online":true,"version":"0.1.1",
 "last_message":"2026-01-15T10:00:30.643951578Z"}
```

`allow`: the destinations the peer may reach through this node (networks with
an optional port or range, separated by commas; empty: any). `udp_dial`:
where this node sends UDP feedback for sessions with the peer (empty: none).

### POST /api/v1/peers/{name}/check

Tries every path to the peer.

```
{"peer":"nas-a","paths":[{"name":"default","addr":"nas-a.example.org:9851","security":"tls","ok":true}]}
```

A path that fails has `"ok": false` and an `"error"`.

### DELETE /api/v1/peers/{name}

Ends the pairing on this node; the sessions with the peer are reset. `204`.

## Rules

### GET /api/v1/rules

```
[{"name":"rsync","listen":"0.0.0.0:9801","destination":"192.0.2.10:7000","peer":"nas-a",
  "enabled":true,"listening":true,"sessions":0},
 {"name":"zfs","listen":"0.0.0.0:50000-50002","destination":"192.0.2.10:50100-50102","peer":"nas-a",
  "enabled":true,"listening":true,"sessions":1}]
```

A transparent rule has `match` in the place of `listen` and `destination`.
`error` is present when the rule does not listen, or when its last connection
could not be carried.

### POST /api/v1/rules

Body `{"name", "peer", "listen", "destination"}` or `{"name", "peer",
"match"}`, and optionally `"enabled": false`.

```
$ curl -X POST ... -d '{"name":"pg","peer":"nas-a","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432"}' http://127.0.0.1:9850/api/v1/rules
HTTP/1.1 201 Created

{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":true,"listening":true,"sessions":0}
```

A rule that cannot be stored:

```
HTTP/1.1 400 Bad Request

{"error":"invalid: rule \"bad\": destination \"example.org:80\": \"example.org\" is not an IP address (host names are not supported yet)"}
```

### PUT /api/v1/rules/{name}

Replaces the rule; same body. Switching a rule off is a `PUT` with
`"enabled": false`:

```
{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":false,"listening":false,"sessions":0}
```

### DELETE /api/v1/rules/{name}

Removes the rule. `204`. Connections it carries run to their end.

## Login (for the web page)

### POST /api/v1/login

Body `{"password": "..."}`. Needs no token.

```
HTTP/1.1 204 No Content
Set-Cookie: tcpraid_session=d4719741...; Path=/; Max-Age=43200; HttpOnly; SameSite=Strict
```

A wrong password is `401`; after five in a row from one address, `429` for a
minute.

A request authorized by this cookie that changes something (any method but
`GET` and `HEAD`) must also carry the header `X-Tcpraid-Request: 1`;
without it the answer is `403`.

### POST /api/v1/logout

Ends the login. `204`.
