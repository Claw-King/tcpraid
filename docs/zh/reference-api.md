# 本机 HTTP API

节点在 `-api` 指定的地址上提供一个 HTTP API(默认 `127.0.0.1:9850`)。各条命令和网页界面都是它的客户端，所以它们能做的事，主机上的其他程序用同样的方式也能做。

这里的示例是这个版本的节点的真实回答，其中的地址、时间和很长的值为了排版做过替换。JSON 在这里分成了几行显示，节点发送时是一行。

## 访问

除了 `/healthz` 和网页界面的文件，每个路径都需要节点的访问令牌，它在数据目录的 `api.token` 里：

```
$ curl -H "Authorization: Bearer $(cat /var/lib/tcpraid/api.token)" http://127.0.0.1:9850/api/v1/status
```

不带令牌，或者令牌不对，回答都是 `401`，不管这个路径存在与否：

```
HTTP/1.1 401 Unauthorized
Www-Authenticate: Bearer

{"error":"missing or wrong access token"}
```

已经登录网页界面的浏览器改由它的 cookie 授权(见文末的“登录”)。

API 是明文 HTTP，除非节点带 `-api-tls` 或 `-api-cert` 启动。那时它在同一个端口上是 HTTPS，不带 TLS 的请求不被服务。API 出示的证书在数据目录的 `api.tls` 里：

```
$ curl --cacert /var/lib/tcpraid/api.tls -H "Authorization: Bearer $(cat /var/lib/tcpraid/api.token)" https://127.0.0.1:9850/api/v1/status
```

节点自己生成的证书里写着 `localhost`、`127.0.0.1`、`::1`、主机名，以及 `-api` 的地址(当它指明了一个地址时)。用别的名字访问 API 的程序，需要你用 `-api-cert` 给出自己的证书。这时要用那张证书里有的名字来访问 API。

错误以 JSON 返回，形式是 `{"error": "..."}`。状态码 `400` 表示请求本身不对，`404` 表示路径或名字不存在，`405` 表示这个路径没有这种方法，`409` 表示路径测试现在不能开始，`502` 表示另一个节点没有完成配对，`504` 表示配对超时。

## 节点

### GET /healthz

`200` 和 `ok`。不需要令牌。

### GET /api/v1/status

```
{"version":"0.11.2","wire_version":1,"started":"2026-01-15T10:00:00.634920968Z","uptime_seconds":11,
 "node":{"name":"nas-b","fingerprint":"cb5a2afa6a827183541b967ed101aba925e7b9289447e21cdf5327599e688e83"},
 "sessions":2,"peers":1,"rules":2,"listen":["0.0.0.0:9851"],"advertise":["nas-b.example:9851"]}
```

`listen` 和 `advertise` 是 0.8.0 版新增的。`listen` 是节点接受其他节点 TLS 连接的地址，按实际绑定的写，没有 `-listen` 的节点是空的。`advertise` 是 `-advertise` 给的地址，申请配对码时不带地址就用它们。两者都是列表，旧版本节点的回答里没有这两项。`metrics` 是 0.10.0 版新增的，是节点不要令牌提供指标的地址，按实际绑定的写。节点启动时没有带 `-metrics` 就没有这一项。

### PUT /api/v1/node

给节点改名。已配对的对端保留的仍是配对当时节点的名字。

```
$ curl -X PUT ... -d '{"name":"nas-a"}' http://127.0.0.1:9850/api/v1/node
{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86"}
```

### GET /api/v1/sessions

正在承载的连接。在接下应用连接的节点上，`role` 是 `client`，这个节点还会给出 `rule`。在另一端的节点上，`role` 是 `server`。

```
[{"id":"4fce0d73c066738a655b88c7741c2405","destination":"192.0.2.10:7000","role":"client",
  "started":"2026-01-15T10:00:31.329910062Z","tx_bytes":314572800,"rx_bytes":0,"unacked_bytes":130023424,
  "resent_bytes":0,"subs":8,"subs_dialed":8,"peer":"nas-a","rule":"rsync","unsent_bytes":100663296,
  "inflight_bytes":29360128,"up":true,"silent_ms":41,"grace_left_ms":899959,"window_bytes":134217728}]
```

`tx_bytes` 和 `rx_bytes` 是已发送和已接收的流字节数。`unacked_bytes` 是已从应用读进来、另一端还没有报告收到的部分，不管它是已经发出去了还是还在等着发。`resent_bytes` 统计排队等待重发的字节数，`subs` 是存活的子连接数。

`rule` 之后的字段是 0.3.0 版新增的：

| 字段 | 含义 |
|---|---|
| `unsent_bytes` | `unacked_bytes` 里还从没发出去过、在等子连接的部分。这个数一直很大，说明应用给数据的速度比线路能带的快 |
| `inflight_bytes` | `unacked_bytes` 的其余部分，至少发过一次，对端还没有报告收到 |
| `up` | 对端至少在一条子连接上应答过。还在建立连接的会话是 `false` |
| `silent_ms` | 上一次从对端收到东西是多久以前。会话连着的时候，它保持在一秒左右以内，因为两个节点至少每秒互相报告一次 |
| `grace_left_ms` | 如果一直收不到东西，会话还会保留多久。它等于保持时间(`-grace`)减去 `silent_ms`。到零时会话结束，应用的连接被重置 |
| `test` | 路径测试(`POST /api/v1/peers/{name}/test`)的会话上才有，值是 `true`。这种会话没有规则，目的地显示为 `0.0.0.0:0` |

`subs_dialed` 是 0.6.0 版新增的。它是这个会话活着的子连接里由本节点打开的条数，其余的由对端打开。规则带 `"dial": "both"` 时，只要两个节点的路径都通，会话的这两个数都不是零。

`window_bytes` 是 0.7.0 版新增的。它是这个会话在本节点开始时取得的窗口：节点的窗口，或者它的对端或规则设的窗口(见“传输层的设置”)。

### GET /metrics

节点的各项数字，Prometheus 文本格式，前缀是 `tcpraid_`：传输层的计数器，每个对端是否在线，每条规则是否在监听、承载着几个会话，进程的内存，以及带 `-hook` 时钩子的情况。`reference-files.md` 的“各项数字和钩子”一节列出了它们。和 API 的每个路径一样，它需要令牌。带 `-metrics` 启动的节点在一个单独的地址上提供同样的数字，不需要令牌，在那里还有 JSON 形式的 `/metrics.json`。

## 配对码

### POST /api/v1/pairing-codes

请求体 `{"addresses": ["host:port", ...], "ttl_seconds": N}`，两项都可选，默认分别是节点的 `-advertise` 地址和一小时。回答里带着配对码，以后不会再显示。

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

和生成配对码的节点配对。请求体 `{"code": "...", "address": "host:port"}`。地址可选，给了就取代配对码里的地址。回答是新的对端，形式见下。

### GET /api/v1/peers, GET /api/v1/peers/{name}

```
[{"name":"nas-a","fingerprint":"e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86",
  "addresses":["nas-a.example.org:9851"],"paired_at":"2026-01-15T10:00:20Z",
  "paths":[{"name":"default","type":"tcp","addr":"nas-a.example.org:9851","security":"tls","roles":["data"],"subs":8}],
  "allow":"","udp_dial":"","online":true,"version":"0.11.2","last_message":"2026-01-15T10:00:26.938712846Z"}]
```

`online` 表示和这个对端之间有一条控制连接连着。`version` 和 `last_message` 在收到过对端的消息之后才出现。0.6.0 版新增的 `spares` 是两个节点之间正在等会话的备用连接数，没有时不出现。0.7.0 版新增的 `transport` 是本节点上和这个对端之间的会话用来取代节点设置的那些传输层设置，对端没有设时不出现(见“传输层的设置”)。0.11.0 版新增的 `features` 列出控制连接连上时对端说它会做的事，用的是 `PROTOCOL.md` 里的名字。同一版新增的 `problem` 说明为什么和一个在线的对端之间什么都不承载，没有这样的原因时不出现。加入到本节点的对端，在这里没有 `addresses`，也没有 `paths`。配对密钥绝不会出现在回答里，显示代理时不带它的用户名和密码。从 0.10.0 版起，代理在节点里有用户名和密码的路径会带 `"proxy_login": true`。这个字段只供读取，写在请求里不改变任何东西。

从 0.5.0 版起，路径可以多两个字段。`addrs` 是同一个监听的其余地址，节点在 `addr` 之后按顺序尝试它们，连同 `addr` 最多 8 个。`mark` 是这条路径的连接带的套接字标记，只在 Linux 上有。只有一个地址、不带标记的路径没有这两个字段：

```
{"name":"default","type":"tcp","addr":"[2001:db8::10]:9851","addrs":["203.0.113.7:9851"],"security":"tls","roles":["data"],"subs":8,"mark":7}
```

### PUT /api/v1/peers/{name}

修改对端的设置。请求体 `{"paths": [...], "allow": "...", "udp_dial": "...", "transport": {...}}`，每一项都可选，至少要有一项。`paths` 替换全部路径，每条的形式和 `GET` 显示的一样。把一条路径连同显示出来的代理原样发回去，它已有的用户名和密码会保留。所有内容先整体检查，有任何一处存不了，就什么都不改。回答是这个对端。

```
$ curl -X PUT ... -d '{"allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":""}' http://127.0.0.1:9850/api/v1/peers/nas-b
{"name":"nas-b","fingerprint":"cb5a2afa...9e688e83","addresses":[],"paired_at":"2026-01-15T10:00:20Z","paths":[],
 "allow":"192.0.2.10/32:7000,192.0.2.10/32:50100-50102","udp_dial":"","online":true,"version":"0.11.2",
 "last_message":"2026-01-15T10:00:30.643951578Z"}
```

`allow` 是对端经由本节点可以访问的目的地，写成网段，可带端口或端口段，用逗号分隔。留空表示不限。`udp_dial` 是本节点为和这个对端之间的会话发送 UDP 回执的地址。留空表示不发。

从 0.7.0 版起，`transport` 按名字设置和这个对端之间会话的传输层设置。带值的名字被设上，值是 `null` 的名字回到节点的值，请求里没有提到的名字保持原样：

```
$ curl -X PUT ... -d '{"transport":{"window":"8MB","grace":null}}' http://127.0.0.1:9850/api/v1/peers/nas-b
{"name":"nas-b","fingerprint":"9d875c73...b9a2cd07","addresses":[],"paired_at":"2026-01-15T10:00:20Z","paths":[],
 "allow":"","udp_dial":"","online":true,"version":"0.11.2","last_message":"2026-01-15T10:00:28.39632723Z",
 "spares":2,"transport":{"window":"8MB"}}
```

### PUT /api/v1/peers/{name}/paths/{path}, DELETE /api/v1/peers/{name}/paths/{path}

0.4.0 版新增。`PUT` 修改对端的一条路径，对端没有这个名字的路径时就新建。请求体里可以有 `addr`、`type`、`proxy`、`security`、`verify`、`pin`、`roles`、`subs` 中的任意几项，至少一项。从 0.5.0 版起还可以有 `addrs` 和 `mark`。只给 `addr` 而不给 `addrs` 的请求会去掉其余的地址，`addrs` 给空列表也一样，`"mark": 0` 去掉标记。请求体没有提到的设置保持原样。新路径必须给 `addr`，没有提到的设置取 `tcp`、`tls`、`data` 角色和 8 条子连接。回答是 `{"created": ..., "peer": {...}}`，里面是保存之后的对端。新建时状态是 `201`，修改时是 `200`。

```
$ curl -X PUT ... -d '{"subs":4}' http://127.0.0.1:9850/api/v1/peers/nas-a/paths/default
{"created":false,"peer":{"name":"nas-a","fingerprint":"e5645f9b...7795ec86","addresses":["nas-a.example.org:9851"],
 "paired_at":"2026-01-15T10:00:20Z",
 "paths":[{"name":"default","type":"tcp","addr":"nas-a.example.org:9851","security":"tls","roles":["data"],"subs":4}],
 "allow":"","udp_dial":"","online":true,"version":"0.11.2","last_message":"2026-01-15T10:00:30.643951578Z"}}
```

`DELETE` 删除这条路径，回答是对端。对端没有这条路径时是 `404`。

对这两种请求，节点读出和写回对端的路径是一步完成的，所以同时到达、改的是同一个对端两条路径的请求都会生效。整份替换的 `PUT`(`PUT /api/v1/peers/{name}` 带 `paths`)保证不了这一点，因为它送来的那份列表是早先读出来的。

### POST /api/v1/peers/{name}/check

逐条尝试通往对端的每条路径。

```
{"peer":"nas-a","paths":[{"name":"default","addr":"nas-a.example.org:9851","security":"tls","ok":true}]}
```

不通的路径是 `"ok": false`，并带一个 `"error"`。从 0.5.0 版起，有多个地址的路径每个地址各有一项，按地址的顺序排列，`name` 相同。

### POST /api/v1/peers/{name}/test

运行一次路径测试。节点向对端发送测试数据，对端收到后丢弃。每条承载数据的路径测两次，先只用一条数据子连接，再用这条路径配置的全部条数。测试结束后才返回回答，每条路径要跑两轮。调用方断开连接时，测试提前结束。

请求体是 `{"seconds": N, "path": "NAME"}`，两项都可以不给。`seconds` 是每一轮持续多久，取 1 到 60，不给时是 10。一轮发满 1 GiB 也会结束。`path` 把测试限定在一条路径上。

```
$ curl -X POST ... -d '{"seconds":10}' http://127.0.0.1:9850/api/v1/peers/nas-a/test
{"peer":"nas-a","seconds":10,"peer_dials":false,"paths":[{"name":"default","subs":8,
  "one_bytes_per_second":2726297.6,"all_bytes_per_second":20761804.8,"little_gain":false}]}
```

| 字段 | 含义 |
|---|---|
| `peer_dials` | 本节点没有通往对端的路径时是 `true`。这时列出的是对端通往本节点的路径，子连接由对端打开 |
| `subs` | 这条路径配置的数据子连接数 |
| `one_bytes_per_second`、`all_bytes_per_second` | 对端报告收到的速度，取每一轮后一半时间的平均。路径只有一条子连接时，第二个数是 0 |
| `little_gain` | 全部子连接合起来的速度不到一条的 1.5 倍时是 `true` |
| `error` | 这条路径没能测出来时才有，这时两个速度都是 0 |

一个节点同一时间只跑一次测试。测试不能开始时，回答是 `409` 加上原因，例如已有一次测试在跑、对端没有连上，或者对端的版本早于 0.3.0。测试数据会和同时在跑的传输争用线路。

### DELETE /api/v1/peers/{name}

在本节点上解除配对，并重置和这个对端之间的会话。`204`。指向这个对端的规则会保留。

带上 0.4.0 版新增的查询参数 `rules` 时，回答是 `200`，并列出指向这个对端的规则。`?rules=keep` 保留它们，`?rules=remove` 把它们和对端一起删除：

```
$ curl -X DELETE ... 'http://127.0.0.1:9850/api/v1/peers/nas-a?rules=remove'
{"peer":"nas-a","rules":["rsync","zfs"],"rules_removed":true}
```

## 规则

### GET /api/v1/rules

```
[{"name":"rsync","listen":"0.0.0.0:9801","destination":"192.0.2.10:7000","peer":"nas-a",
  "enabled":true,"listening":true,"sessions":0},
 {"name":"zfs","listen":"0.0.0.0:50000-50002","destination":"192.0.2.10:50100-50102","peer":"nas-a",
  "enabled":true,"listening":true,"sessions":1}]
```

透明规则用 `match` 代替 `listen` 和 `destination`。SOCKS5 规则(0.9.0 版新增)带的是 `socks`，也就是它监听 SOCKS5 客户端的地址。规则没有在监听，或者它最近一条连接没能被承载时，会出现 `error`。0.3.0 版新增的 `error_at` 是这件事发生的时间：

```
{"name":"rsync","listen":"0.0.0.0:9801","destination":"192.0.2.10:7000","peer":"nas-a",
 "enabled":true,"listening":true,"sessions":0,
 "error":"peer \"nas-a\" could not connect to 192.0.2.10:7000: connect: connection refused",
 "error_at":"2026-01-15T10:02:11.740399432Z"}
```

`error` 说的是眼下的状态而不是一件已经发生的事时，没有 `error_at`，例如规则的一条连接还在等对端应答。这条规则的下一条连接到来时，两个字段都会清掉。

0.5.0 版新增了三个字段，没有内容时都不出现。`paths` 和 `subs` 是规则被限定用的路径和子连接数(见 `POST`)。本节点自己通往对端的路径不通、规则的连接改由对端打开的期间，`peer_dials` 是 `true`：

```
{"name":"pull","listen":"0.0.0.0:9802","destination":"192.0.2.10:873","peer":"nas-a",
 "enabled":true,"listening":true,"sessions":0,"paths":["side"],"subs":3}
```

0.6.0 版新增了 `dial`。规则指定了节点之间的连接由谁打开时，它是 `"self"`、`"peer"` 或 `"both"`(见 `POST`)。规则把这件事交给节点决定时，也就是 `"auto"`，这个字段不出现。

0.7.0 版新增了 `transport`，是这条规则的连接用来取代对端和节点设置的那些传输层设置。规则没有设时不出现：

```
{"name":"zfs","listen":"0.0.0.0:50000-50002","destination":"192.0.2.10:50100-50102","peer":"nas-a",
 "enabled":true,"listening":true,"sessions":0,"transport":{"window":"16MB"}}
```

### POST /api/v1/rules

请求体 `{"name", "peer", "listen", "destination"}` 或 `{"name", "peer", "match"}`，还可以带 `"enabled": false`。从 0.4.0 版起，目的地里 IP 地址的位置可以写主机名，例如 `"nas.lan:873"`，由对端在每条连接到来时解析。

从 0.5.0 版起，请求体里可以有 `"paths": ["NAME", ...]` 和 `"subs": N`。给了 `paths`，规则的连接只用这些名字的路径，名字以持有路径的节点起的为准。给了 `subs`，每条连接在每条路径上最多保持这么多条数据子连接。在持有路径的节点上，名字不是一条承载数据的路径时回答 `400`。由对端持有路径时，名字由对端在连接到来时核对，对端要是 0.5.0 或更新的版本。

从 0.6.0 版起，请求体里可以有 `"dial"`，取 `"auto"`、`"self"`、`"peer"`、`"both"` 之一，指定规则的会话里节点之间的连接由哪个节点打开。四个取值的含义见 `reference-cli.md` 里 `rules add -dial` 的说明。不是这四个之一的值回答 `400`。`dial` 需要而两个节点不具备的条件，要到连接到来时才看得出，写在规则的 `error` 里。

从 0.7.0 版起，请求体里可以有 `"transport": {"NAME": "VALUE", ...}`，是这条规则的连接的传输层设置(见“传输层的设置”)。不是设置的名字、超出范围的值、节点的全部会话共用的设置，都回答 `400`。

从 0.9.0 版起，请求体 `{"name", "peer", "socks"}` 新建一条 SOCKS5 规则。`socks` 写成 `"IP:port"`，或者用 `":port"` 表示回环地址 `127.0.0.1`，回答里是保存下来的那个地址。这种规则在 `reference-cli.md` 的“SOCKS5 规则”里说明。

```
$ curl -X POST ... -d '{"name":"pg","peer":"nas-a","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432"}' http://127.0.0.1:9850/api/v1/rules
HTTP/1.1 201 Created

{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":true,"listening":true,"sessions":0}
```

存不了的规则：

```
HTTP/1.1 400 Bad Request

{"error":"invalid: rule \"bad\": destination \"192.0.2:80\": \"192.0.2\" cannot be used: it is neither an IP address nor a host name, whose last part is never a number"}
```

### PUT /api/v1/rules/{name}

替换这条规则，请求体相同。`paths`、`subs`、`dial` 和 `transport` 是例外：请求体没有提到其中哪一项，规则就保留原有的那一项，这样不认识这几个字段的调用方不会把它们去掉。`paths` 给空列表、`subs` 给 `0`、`dial` 给 `"auto"` 才是去掉。`transport` 里的每个名字各算各的：带值的被设上，值是 `null` 的被去掉，没有提到的保持原样。只改 `transport` 时，规则的监听不动。从 0.9.0 版起，请求体里 `listen`、`destination`、`match` 和 `socks` 一个都没有时，这几项保持规则原有的样子，这样不认识某一种规则的调用方仍然可以启用和停用它。停用一条规则就是带 `"enabled": false` 的 `PUT`：

```
{"name":"pg","listen":"127.0.0.1:15432","destination":"192.0.2.10:5432","peer":"nas-a","enabled":false,"listening":false,"sessions":0}
```

### DELETE /api/v1/rules/{name}

删除这条规则。`204`。它正在承载的连接会跑完。

## connect

### POST /api/v1/connect

0.9.0 版新增。节点把发来这个请求的那条连接本身，作为一个会话，送到对端那边的一个目的地。`tcpraid connect` 用的就是它(见 `reference-cli.md`)。

请求体 `{"peer": "NAME", "host": "HOST", "port": N}`。`host` 是 IP 地址或主机名，主机名由对端解析。这个请求和别的请求一样需要令牌，用不了的请求体和别处一样回答 `400`。在对端那里，目的地要过对端为本节点保存的允许列表。

到目的地的连接开始时，节点回答

```
HTTP/1.1 101 Switching Protocols
Connection: Upgrade
Upgrade: tcpraid-connect
Tcpraid-Session: 5f0c1a7e93b24d6688aa0c3e51d2f7b9
```

从这几行头之后的空行起，这条连接上双向传的只有数据流本身。0.11.0 版新增的 `Tcpraid-Session` 是承载这条连接的会话的编号。两个方向都完整结束的流，以连接的正常关闭结束。被切断的流以重置结束，经 TLS 时不发关闭 TLS 连接的那个告警，所以程序绝不会把被切断的流的结尾读成数据的结尾。

到目的地的连接没有开始时，节点回答原因并关闭连接：

```
HTTP/1.1 403 Forbidden

{"error":"peer \"nas-a\" refused the destination 192.0.2.10:7, which is not in the allow list it keeps for this node. Change the list on that node with \"tcpraid peers set\"."}
```

`403` 是对端的允许列表。其余原因都是 `502`：对端没有配对或没有连着，目的地连不上，名字没有地址。

### GET /api/v1/sessions/{id}/end

0.11.0 版新增。它回答这个编号的会话在本节点上是怎么结束的，会话还在进行时就等到它结束。由 `POST /api/v1/connect` 拿到连接的程序，在连接的两个方向都结束之后问它，就知道自己发的东西是不是都传过去了。

```
$ curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9850/api/v1/sessions/5f0c1a7e93b24d6688aa0c3e51d2f7b9/end
{"ended":true,"complete":true,"reason":"done"}
```

| 字段 | 含义 |
|---|---|
| `ended` | 在请求等待的时间里会话还没有结束时是 `false`。这时再问一次 |
| `complete` | 数据流在两个方向上都完整结束了。另一个节点把这一端的全部数据写进了它通往目的地的连接，并按顺序结束了那个方向，目的地的全部数据也已经在本节点交了出去 |
| `reason` | 原因，和节点日志里写的一样 |

请求等 30 秒，或者等查询串里写的秒数(`?wait=10`)，最多 60 秒。`404` 表示节点没有这个编号的会话，也不记得有这样一个会话结束过。节点把最近 256 个会话的结束情况保留 5 分钟。`complete` 为 `false` 的会话是被切断的，或者另一个节点还没说它已经把数据全部传过去就不在了。

## 传输层的设置

`tcpraid run` 作为选项接受的那些传输层设置(见 `reference-files.md`)，可以在节点运行中读取和修改。它们是 0.7.0 版新增的。每个值都是文本，写法和命令行上一样：大小写成 `"256MB"`，时间写成 `"500ms"`，数字写成 `"5"`，开关写成 `"true"` 或 `"false"`。节点回答时用它保存的写法，所以 `"0.5s"` 回来是 `"500ms"`。

一个会话在开始的那一刻取得每一项设置：规则设了就用规则的，否则用对端的，再否则用节点的。改动只对之后开始的会话起作用。

### GET /api/v1/transport

节点的设置，顺序固定。

```
{"settings":[
 {"name":"window","value":"32MB","default":"128MB","source":"stored","stored":"32MB"},
 {"name":"grace","value":"15m0s","default":"15m0s","source":"default"},
 {"name":"hol","value":"1s","default":"1s","source":"default"},
 {"name":"target","value":"300ms","default":"300ms","source":"default"},
 {"name":"pigtarget","value":"1s","default":"1s","source":"default"},
 {"name":"dead","value":"30s","default":"20s","source":"stored","stored":"30s"},
 {"name":"kickratio","value":"5","default":"5","source":"default"},
 {"name":"kickfloor","value":"256KB","default":"256KB","source":"default"},
 {"name":"share","value":"true","default":"true","source":"default"},
 {"name":"fbsubs","value":"3","default":"3","source":"default"},
 {"name":"fbevery","value":"20ms","default":"20ms","source":"default"},
 {"name":"coalesce","value":"20ms","default":"20ms","source":"default"},
 {"name":"budget","value":"256MB","default":"256MB","source":"default","shared":true},
 {"name":"timeline","value":"0s","default":"0s","source":"default","shared":true}]}
```

| 字段 | 含义 |
|---|---|
| `value` | 此刻开始的会话从节点拿到的值 |
| `default` | 程序的默认值 |
| `source` | `"default"`；`tcpraid run` 启动时带了这项设置是 `"command line"`；节点运行中设的是 `"stored"` |
| `stored` | 存在节点数据目录里的值。`"source": "command line"` 时它也可能在，表示有一个存下的值在等一次不带这个选项的启动 |
| `shared` | 节点的全部会话共用的设置上是 `true`。对端和规则不能有这项设置 |

### PUT /api/v1/transport

请求体 `{"set": {"NAME": "VALUE", ...}}`。带值的名字被设上，值是 `null` 的名字回到默认值。至少要有一个名字。回答是 `GET` 给出的那个列表。这些值会被保存，重启之后仍然有效。

```
$ curl -X PUT ... -d '{"set":{"window":"32MB","dead":"30s"}}' http://127.0.0.1:9850/api/v1/transport
```

下面这些回答 `400`，并且什么都不改：不是设置的名字，超出范围的值，和节点的 `-maxrss` 对不上的 `budget`，以及节点的命令行给出的设置：

```
HTTP/1.1 400 Bad Request

{"error":"invalid: the node was started with -grace 20m0s on its command line, which goes before a stored value. Start the node without -grace to set it while the node runs."}
```

## 登录(供网页界面使用)

### POST /api/v1/login

请求体 `{"password": "..."}`。不需要令牌。从 0.8.0 版起必须带请求头 `X-Tcpraid-Request: 1`，不带的话在查看密码之前就回答 `403`。

```
HTTP/1.1 204 No Content
Set-Cookie: tcpraid_session_9850=d4719741...; Path=/; Max-Age=43200; HttpOnly; SameSite=Strict
```

从 0.8.0 版起，cookie 的名字以请求的 `Host` 头里的端口结尾，这样同一台主机上两个端口的两个节点的登录互不相干。`Host` 里没有端口的请求得到的名字是 `tcpraid_session`。经 TLS 时 cookie 还带 `Secure`。

密码错误是 `401`。同一个地址连续错五次之后，一分钟内的回答是 `429`。节点对同一个地址一次只核对一个密码，总共同时核对四个。某个地址的密码正在核对时，它的第二次登录是 `429`。已有四个在核对时，新的登录是 `503`。两者都带 `Retry-After: 1`。

由这个 cookie 授权、并且会修改东西的请求，也就是 `GET` 和 `HEAD` 以外的任何方法，还必须带上请求头 `X-Tcpraid-Request: 1`。不带的话回答是 `403`。

### POST /api/v1/logout

结束登录。`204`。

带 `-no-ui` 启动的节点没有网页界面。在这样的节点上，登录接口和页面文件的路径回答 `404`，cookie 不授权任何请求。
