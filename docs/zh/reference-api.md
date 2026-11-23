# 本机 HTTP API

节点在 `-api` 指定的地址上提供一个 HTTP API(默认 `127.0.0.1:9850`)。各条命令和网页界面都是它的客户端；它们能做的事，主机上的其他程序用同样的方式也能做。

这里的示例是这个版本的节点的真实回答，其中的地址、时间和很长的值为了排版做过替换。JSON 在这里分成了几行显示；节点发送时是一行。

## 访问

除了 `/healthz`(以及网页界面的文件)，每个路径都需要节点的访问令牌，它在数据目录的 `api.token` 里：

```
$ curl -H "Authorization: Bearer $(cat /var/lib/tcpraid/api.token)" http://127.0.0.1:9850/api/v1/status
```

不带令牌，或者令牌不对，回答都是 `401`，不管这个路径存在与否：

```
HTTP/1.1 401 Unauthorized
Www-Authenticate: Bearer

{"error":"missing or wrong access token"}
```

已经登录网页界面的浏览器改由它的 cookie 授权(见文末的“登录”)。API 是明文 HTTP。

错误以 JSON 返回，`{"error": "..."}`：`400` 表示请求本身不对，`404` 表示路径或名字不存在，`405` 表示这个路径没有这种方法，`502` 表示另一个节点没有完成配对，`504` 表示配对超时。

## 节点

### GET /healthz

`200` 和 `ok`。不需要令牌。

### GET /api/v1/status

```
{"version":"0.1.0","wire_version":1,"started":"2026-01-15T10:00:00.634920968Z","uptime_seconds":11,
 "node":{"name":"nas-b","fingerprint":"cb5a2afa6a827183541b967ed101aba925e7b9289447e21cdf5327599e688e83"},
 "sessions":2,"peers":1,"rules":2}
```

### PUT /api/v1/node

给节点改名。已配对的对端保留的仍是配对当时节点的名字。

```
$ curl -X PUT ... -d '{"name":"nas-a"}' http://127.0.0.1:9850/api/v1/node
{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86"}
```

### GET /api/v1/sessions

正在承载的连接。在接下应用连接的那个节点上，`role` 是 `client`(这个节点还会给出 `rule`)；在另一端的节点上是 `server`。

```
[{"id":"4fce0d73c066738a655b88c7741c2405","destination":"192.0.2.10:7000","role":"client",
  "started":"2026-01-15T10:00:31.329910062Z","tx_bytes":314572800,"rx_bytes":0,"unacked_bytes":0,
  "resent_bytes":0,"subs":8,"peer":"nas-a","rule":"rsync"}]
```

`tx_bytes` 和 `rx_bytes` 是已发送和已接收的流字节数；`unacked_bytes` 是另一端还没有报告收到的部分；`resent_bytes` 统计排队等待重发的字节数；`subs` 是存活的子连接数。

### GET /metrics

Prometheus 文本格式的计数器，前缀是 `tcpraid_`。

## 配对码

### POST /api/v1/pairing-codes

请求体 `{"addresses": ["host:port", ...], "ttl_seconds": N}`，两项都可选(默认：节点的 `-advertise` 地址，一小时)。回答里带着配对码，只有这一次。

```
$ curl -X POST ... -d '{"ttl_seconds":600}' http://127.0.0.1:9850/api/v1/pairing-codes
HTTP/1.1 201 Created
Cache-Control: no-store

{"id":"cb9c5e95","expires":"2026-01-15T10:10:30Z","used":false,"code":"tcpraid-pair-1-zoof5fkv3d4x..."}
```

### GET /api/v1/pairing-codes

尚未过期的配对码，不含配对码本身。

```
[{"id":"a101905f","expires":"2026-01-15T11:00:20Z","used":true},
 {"id":"cb9c5e95","expires":"2026-01-15T10:10:30Z","used":false}]
```

### DELETE /api/v1/pairing-codes/{id}

作废一个配对码。`204`。

## 对端

### POST /api/v1/peers

和生成配对码的那个节点配对。请求体 `{"code": "...", "address": "host:port"}`；地址可选，给了就取代配对码里的地址。回答是新的对端，形式见下。

### GET /api/v1/peers, GET /api/v1/peers/{name}

```
[{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86",
  "addresses":["nas-a.example.org:9851"],"paired_at":"2026-01-15T10:00:20Z",
  "paths":[{"name":"default","type":"tcp","addr":"nas-a.example.org:9851","security":"tls","roles":["data"],"subs":8}],
  "allow":"","udp_dial":"","online":true,"version":"0.1.0","last_message":"2026-01-15T10:00:26.938712846Z"}]
```

`online`：和这个对端之间有一条控制连接连着。`version` 和 `last_message` 在收到过对端的消息之后才出现。加入到本节点的对端，在这里没有 `addresses`，也没有 `paths`。配对密钥绝不会出现在回答里，显示代理时不带它的用户名和密码。

### PUT /api/v1/peers/{name}

修改对端的设置。请求体 `{"paths": [...], "allow": "...", "udp_dial": "..."}`，每一项都可选，至少要有一项。`paths` 替换全部路径，每条的形式和 `GET` 显示的一样；把一条路径连同显示出来的代理原样发回去，它已有的用户名和密码会保留。所有内容先整体检查；有任何一处存不了，就什么都不改。回答是这个对端。

```
$ curl -X PUT ... -d '{"allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":""}' http://127.0.0.1:9850/api/v1/peers/nas-b
{"name":"nas-b","fingerprint":"cb5a2afa...9e688e83","addresses":[],"paired_at":"2026-01-15T10:00:20Z","paths":[],
 "allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":"","online":true,"version":"0.1.0",
 "last_message":"2026-01-15T10:00:30.643951578Z"}
```

`allow`：对端经由本节点可以访问的目的地(网段，可带端口或端口段，用逗号分隔；留空：不限)。`udp_dial`：本节点为和这个对端之间的会话发送 UDP 回执的地址(留空：不发)。

### POST /api/v1/peers/{name}/check

逐条尝试通往对端的每条路径。

```
{"peer":"nas-a","paths":[{"name":"default","addr":"nas-a.example.org:9851","security":"tls","ok":true}]}
```

不通的路径是 `"ok": false`，并带一个 `"error"`。

### DELETE /api/v1/peers/{name}

在本节点上解除配对；和这个对端之间的会话被重置。`204`。

## 规则

### GET /api/v1/rules

```
[{"name":"rsync","listen":"0.0.0.0:9801","destination":"192.0.2.10:7000","peer":"nas-a",
  "enabled":true,"listening":true,"sessions":0},
 {"name":"zfs","listen":"0.0.0.0:50000-50002","destination":"192.0.2.10:50100-50102","peer":"nas-a",
  "enabled":true,"listening":true,"sessions":1}]
```

透明规则用 `match` 代替 `listen` 和 `destination`。规则没有在监听，或者它最近一条连接没能被承载时，会出现 `error`。

### POST /api/v1/rules

请求体 `{"name", "peer", "listen", "destination"}` 或 `{"name", "peer", "match"}`，还可以带 `"enabled": false`。

```
$ curl -X POST ... -d '{"name":"pg","peer":"nas-a","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432"}' http://127.0.0.1:9850/api/v1/rules
HTTP/1.1 201 Created

{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":true,"listening":true,"sessions":0}
```

存不了的规则：

```
HTTP/1.1 400 Bad Request

{"error":"invalid: rule \"bad\": destination \"example.org:80\": \"example.org\" is not an IP address (host names are not supported yet)"}
```

### PUT /api/v1/rules/{name}

替换这条规则；请求体相同。停用一条规则就是带 `"enabled": false` 的 `PUT`：

```
{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":false,"listening":false,"sessions":0}
```

### DELETE /api/v1/rules/{name}

删除这条规则。`204`。它正在承载的连接会跑完。

## 登录(供网页界面使用)

### POST /api/v1/login

请求体 `{"password": "..."}`。不需要令牌。

```
HTTP/1.1 204 No Content
Set-Cookie: tcpraid_session=d4719741...; Path=/; Max-Age=43200; HttpOnly; SameSite=Strict
```

密码错误是 `401`；同一个地址连续错五次之后，一分钟内是 `429`。

由这个 cookie 授权、并且会修改东西的请求(`GET` 和 `HEAD` 以外的任何方法)还必须带上请求头 `X-Tcpraid-Request: 1`；不带的话回答是 `403`。

### POST /api/v1/logout

结束登录。`204`。
