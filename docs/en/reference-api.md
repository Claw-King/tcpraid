# The local HTTP API

A node serves an HTTP API at the address given with `-api` (default
`127.0.0.1:9850`). The commands and the web page are clients of it, so
anything they do, another program on the host can do the same way.

The examples are real answers of a node of this version, with addresses,
times and long values changed for print. JSON is shown on several lines here,
but the node sends it on one.

## Access

Every path except `/healthz` and the files of the web page needs the access
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

Errors are JSON, `{"error": "..."}`. The status is `400` for a request that
cannot be right, `404` for a path or a name that does not exist, `405` for a
method the path does not have, `409` for a path test that cannot start now,
`502` for a pairing that the other node did not complete and `504` for one
that ran out of time.

## Node

### GET /healthz

`200` and `ok`. Needs no token.

### GET /api/v1/status

```
{"version":"0.4.0","wire_version":1,"started":"2026-01-15T10:00:00.634920968Z","uptime_seconds":11,
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
application's connection, and that node also names the `rule`. It is
`server` on the node at the other end.

```
[{"id":"4fce0d73c066738a655b88c7741c2405","destination":"192.0.2.10:7000","role":"client",
  "started":"2026-01-15T10:00:31.329910062Z","tx_bytes":314572800,"rx_bytes":0,"unacked_bytes":130023424,
  "resent_bytes":0,"subs":8,"peer":"nas-a","rule":"rsync","unsent_bytes":100663296,
  "inflight_bytes":29360128,"up":true,"silent_ms":41,"grace_left_ms":899959}]
```

`tx_bytes` and `rx_bytes` are stream bytes sent and received. `unacked_bytes`
is what was read from the application and the other end has not yet reported
received, whether it has been sent already or still waits to be sent.
`resent_bytes` counts the bytes queued for sending again, and `subs` is the
number of live sub-connections.

The fields after `rule` were added in version 0.3.0:

| Field | Meaning |
|---|---|
| `unsent_bytes` | the part of `unacked_bytes` that was never sent yet and waits for a sub-connection. A large value means the application delivers faster than the line carries |
| `inflight_bytes` | the rest of `unacked_bytes`, which was sent at least once and is not reported received |
| `up` | the peer has answered on a sub-connection at least once. `false` on a session that is still connecting |
| `silent_ms` | how long ago anything arrived from the peer. While a session is connected this stays under about a second, because the two nodes report to each other at least that often |
| `grace_left_ms` | how much longer the session is kept if nothing arrives. It is the grace time (`-grace`) minus `silent_ms`. At zero the session ends and the application's connection is reset |
| `test` | present and `true` on the session of a path test (`POST /api/v1/peers/{name}/test`). Such a session has no rule, and its destination reads `0.0.0.0:0` |

### GET /metrics

Counters in the Prometheus text format, prefixed `tcpraid_`.

## Pairing codes

### POST /api/v1/pairing-codes

Body `{"addresses": ["host:port", ...], "ttl_seconds": N}`. Both are
optional and default to the node's `-advertise` addresses and one hour. The
answer holds the code, which is not shown again.

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
"host:port"}`. The address is optional and replaces the addresses in the
code. The answer is the new peer, as below.

### GET /api/v1/peers, GET /api/v1/peers/{name}

```
[{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86",
  "addresses":["nas-a.example.org:9851"],"paired_at":"2026-01-15T10:00:20Z",
  "paths":[{"name":"default","type":"tcp","addr":"nas-a.example.org:9851","security":"tls","roles":["data"],"subs":8}],
  "allow":"","udp_dial":"","online":true,"version":"0.4.0","last_message":"2026-01-15T10:00:26.938712846Z"}]
```

`online` means that a control connection with the peer is up. `version` and
`last_message` appear once the peer has been heard from. A peer that joined
this node has no `addresses` and no `paths` here. The pair secret is never in
an answer, and a proxy is shown without its user and password.

### PUT /api/v1/peers/{name}

Changes a peer's settings. Body `{"paths": [...], "allow": "...", "udp_dial":
"..."}`. Each field is optional, and at least one must be there. `paths`
replaces all paths, each in the form the `GET` shows. A path sent back with
its proxy as shown keeps the user and password it has. Everything is checked
first, and if something cannot be stored, nothing changes. The answer is the
peer.

```
$ curl -X PUT ... -d '{"allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":""}' http://127.0.0.1:9850/api/v1/peers/nas-b
{"name":"nas-b","fingerprint":"cb5a2afa...9e688e83","addresses":[],"paired_at":"2026-01-15T10:00:20Z","paths":[],
 "allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":"","online":true,"version":"0.4.0",
 "last_message":"2026-01-15T10:00:30.643951578Z"}
```

`allow` holds the destinations the peer may reach through this node, as
networks with an optional port or range, separated by commas. Empty means
any. `udp_dial` is where this node sends UDP feedback for sessions with the
peer. Empty means none.

### PUT /api/v1/peers/{name}/paths/{path}, DELETE /api/v1/peers/{name}/paths/{path}

Added in version 0.4.0. The `PUT` changes one path of a peer, or adds it if
the peer has none of that name. Its body holds any of `addr`, `type`,
`proxy`, `security`, `verify`, `pin`, `roles` and `subs`, and at least one.
What the body does not name stays as it is. A new path needs `addr`, and
what the body does not name is then `tcp`, `tls`, the role `data` and 8
sub-connections. The answer is `{"created": ..., "peer": {...}}` with the
peer as stored, and the status is `201` for a new path and `200` for a
change.

```
$ curl -X PUT ... -d '{"subs":4}' http://127.0.0.1:9850/api/v1/peers/nas-a/paths/default
{"created":false,"peer":{"name":"nas-a","fingerprint":"e5645f9b...7795ec86","addresses":["nas-a.example.org:9851"],
 "paired_at":"2026-01-15T10:00:20Z",
 "paths":[{"name":"default","type":"tcp","addr":"nas-a.example.org:9851","security":"tls","roles":["data"],"subs":4}],
 "allow":"","udp_dial":"","online":true,"version":"0.4.0","last_message":"2026-01-15T10:00:30.643951578Z"}}
```

The `DELETE` removes the path and answers with the peer. A peer without that
path is `404`.

The node reads and writes the peer's paths in one step for each of these
requests. Requests for two paths of a peer that arrive at the same time
therefore both take effect. A `PUT` of the whole list
(`PUT /api/v1/peers/{name}` with `paths`) cannot promise that, because the
list it sends was read earlier.

### POST /api/v1/peers/{name}/check

Tries every path to the peer.

```
{"peer":"nas-a","paths":[{"name":"default","addr":"nas-a.example.org:9851","security":"tls","ok":true}]}
```

A path that fails has `"ok": false` and an `"error"`.

### POST /api/v1/peers/{name}/test

Runs a path test. The node sends test data to the peer, which discards it,
and measures each path that carries data twice, first with one data
sub-connection and then with all the path has. The request is answered when
the test is over, which takes two runs for each path. The test ends early if
the caller closes the connection.

Body `{"seconds": N, "path": "NAME"}`. Both are optional. `seconds` is how
long each run lasts, 1 to 60, and 10 if absent. A run also ends after 1 GiB.
`path` limits the test to one path.

```
$ curl -X POST ... -d '{"seconds":10}' http://127.0.0.1:9850/api/v1/peers/nas-a/test
{"peer":"nas-a","seconds":10,"peer_dials":false,"paths":[{"name":"default","subs":8,
  "one_bytes_per_second":2726297.6,"all_bytes_per_second":20761804.8,"little_gain":false}]}
```

| Field | Meaning |
|---|---|
| `peer_dials` | `true` when this node has no path to the peer. The paths listed are then the ones the peer holds to this node, and the peer opened the sub-connections |
| `subs` | the number of data sub-connections the path has |
| `one_bytes_per_second`, `all_bytes_per_second` | what the peer reported as received, averaged over the second half of each run. The second is 0 when the path has one sub-connection |
| `little_gain` | `true` when all sub-connections together carried less than 1.5 times what one carried |
| `error` | present when the path could not be measured. The two rates are 0 then |

A node runs one test at a time. A test that cannot start is answered with
`409` and the reason, for example that another test is running, that the
peer is not connected, or that the peer runs a version before 0.3.0. The
test data competes with the transfers that run at the same time.

### DELETE /api/v1/peers/{name}

Ends the pairing on this node and resets the sessions with the peer. `204`.
The rules that name the peer stay.

With the query `rules`, added in version 0.4.0, the answer is `200` and says
which rules named the peer. `?rules=keep` leaves them, and `?rules=remove`
removes them together with the peer:

```
$ curl -X DELETE ... 'http://127.0.0.1:9850/api/v1/peers/nas-a?rules=remove'
{"peer":"nas-a","rules":["rsync","zfs"],"rules_removed":true}
```

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
could not be carried. `error_at`, added in version 0.3.0, is when that
happened:

```
{"name":"rsync","listen":"0.0.0.0:9801","destination":"192.0.2.10:7000","peer":"nas-a",
 "enabled":true,"listening":true,"sessions":0,
 "error":"peer \"nas-a\" could not connect to 192.0.2.10:7000: connect: connection refused",
 "error_at":"2026-01-15T10:02:11.740399432Z"}
```

`error_at` is absent when `error` describes a state that holds now, such as
a rule whose connection is still waiting for the peer to answer. The next
connection of the rule clears both fields.

### POST /api/v1/rules

Body `{"name", "peer", "listen", "destination"}` or `{"name", "peer",
"match"}`, and optionally `"enabled": false`. Since version 0.4.0 the
destination may hold a host name in the place of the IP address, as in
`"nas.lan:873"`. The peer looks it up for every connection.

```
$ curl -X POST ... -d '{"name":"pg","peer":"nas-a","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432"}' http://127.0.0.1:9850/api/v1/rules
HTTP/1.1 201 Created

{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":true,"listening":true,"sessions":0}
```

A rule that cannot be stored:

```
HTTP/1.1 400 Bad Request

{"error":"invalid: rule \"bad\": destination \"192.0.2:80\": \"192.0.2\" cannot be used: it is neither an IP address nor a host name, whose last part is never a number"}
```

### PUT /api/v1/rules/{name}

Replaces the rule, with the same body. Switching a rule off is a `PUT` with
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

A wrong password is `401`. After five in a row from one address, the answer
is `429` for a minute.

A request authorized by this cookie that changes something, that is, any
method but `GET` and `HEAD`, must also carry the header
`X-Tcpraid-Request: 1`. Without it the answer is `403`.

### POST /api/v1/logout

Ends the login. `204`.

A node started with `-no-ui` has no web page. The login and the paths of the
page's files answer `404` there, and a cookie authorizes nothing.
