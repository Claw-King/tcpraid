# 排错

先看这三处：**规则的状态**(“规则”页，或 `tcpraid rules`)、**对端的状态**(“对端”页，或 `tcpraid peers`)，以及**节点的日志**(`docker logs tcpraid`、`journalctl -u tcpraid-node`)。

## 应用连不上，或者一连上就被断开

规则承载不了某条连接时，会重置它并记下原因：

```
nas-a$ tcpraid rules
NAME  TAKES         PEER   DESTINATION         SESSIONS  STATE
back  0.0.0.0:9811  nas-b  198.51.100.20:7100  0         listening; last connection: this node has no path to peer "nas-b", and the peer is not connected to this node (it would open the connections): start the peer, or give this node a path to it (tcpraid peers path set)

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
taken  0.0.0.0:9700  nas-a  192.0.2.10:873  0         FAILED: listening on 0.0.0.0:9700 failed: listen tcp 0.0.0.0:9700: bind: address already in use
```

| 你看到的 | 含义 |
|---|---|
| `STATE` 是 `FAILED`，后面带原因 | 这条规则没能监听：端口被占用，或者不被允许。其他规则照常运行。任何规则有改动时，节点会再试一次 |
| "this node has no path to peer ... and the peer is not connected" | 对端离线，而本节点没法拨过去。启动对端，检查它通往本节点的路径 |
| 在这一切之前就被拒绝连接(connection refused) | 那个端口上没有东西在监听。在容器里：规则的端口映射出来了吗？规则是不是监听在 `0.0.0.0` 上？ |
| 连接被接受，随即被重置 | 远端节点连不到目的地，或者对端的允许列表里不包含它。远端节点的日志会说明是哪一种：`dial ADDRESS: ... connection refused`，或者 `dial ADDRESS: not in the allow list of the peer`；本节点的日志里是 `server could not reach ADDRESS` |
| 15 秒后被重置，规则在没有路径的那个节点上 | 已经请对端连过来，但什么都没到：对端已经连不到本节点的监听了 |
| 30 秒后被重置，而且规则的状态里没有原因 | 子连接一条都没建起来。原因之一：另一个节点上已经解除了配对。这时 `tcpraid peers check NAME` 会说 "the other node does not accept this node: it is not paired with it, or no longer"，另一个节点的日志里有 "refused a connection from ...: not a peer of this node" 这样的行。重新配对 |

目的地是**从远端节点看过去的**地址。远端节点跑在容器里时，`127.0.0.1` 是容器，不是 NAS。

## 对端离线

`online` 表示两个节点之间的控制连接连着。持有路径的那个节点负责打开它，断了每 8 秒重试一次。

```
nas-b$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok
```

`peers check` 在每条路径上打开一条连接，并让对端证明它持有这个配对。如果失败，报错会说明问题出在哪里：

- `FAILED: dial tcp ...: connect: connection refused`(或者超时)：查地址、途中的端口转发，或者对端的 `-listen` 端口；
- `FAILED: tls: first record does not look like a TLS handshake`：这个地址通向的不是节点的 TLS 监听，例如连到了节点的 9850 端口而不是 9851；
- `FAILED: ... connection reset by peer (the other node closed the connection during the handshake; it may be busy: try again)`，而且每次都这样：除了节点正忙之外，`tls` 档的路径连到节点的明文监听(`-listen-plain`)时得到的也是这个。检查端口和路径的档位是否对应；
- "the node at this address is not the peer: its key has another fingerprint"：在那个地址上应答的是另一个节点，或者对端换了新的身份(它的数据目录丢了)；
- "the other node does not accept this node: it is not paired with it, or no longer"：另一个节点上已经解除了配对；
- "the other end closed the connection without answering"：在 `off` 档的路径上，是另一个节点没有和本节点配对；在任何一档，可能是一次被打断的配对之后两边持有不同的配对密钥，或者两边的时钟相差超过十分钟(这时另一个节点的日志会写明)。重新配对，或者校准时钟。

没有通往对端路径的节点没法检查(`this node has no path to ...: it can only be reached by it`)；请从另一边检查。

## 配对失败

- "no address for the other node to connect to"：节点不知道自己对外的地址。生成配对码时给一个(`-addr HOST:PORT`，或者网页界面里的那个字段)，或者启动节点时加上 `-advertise`。
- "the other node did not accept the pairing code"：配对码已经用过、已经过期(默认一小时)、已被作废，或者没有复制完整。
- "the pairing code is damaged"：重新复制，要完整、不能有改动。
- "the node at this address is not the one that made the pairing code"：这个地址通向的是另一个节点。
- 完成不了的加入会在 60 秒后放弃。

## 能用，但没有变快

- 先在不用 tcpraid 的情况下测线路：用 `iperf3` 分别跑一条流和八条流。如果八条合起来并不明显比一条快，tcpraid 就没有什么可以提升的。
- 看会话(“会话”页，或 `GET /api/v1/sessions`)：`subs` 应该等于各条路径的连接数之和(默认 8)，`resent_bytes` 应该只占 `tx_bytes` 的一小部分。重发的字节很多，说明路径在丢包或者卡顿；这时有帮助的是第二条承担 `repair` 和 `control`(网页上勾“补发”和“回执”)的路径，或者 UDP 回执(见 `concepts.md`)。
- 应用和它的节点之间、远端节点和目的地之间这两段的速度同样算数：tcpraid 快不过其中较慢的一段，也快不过应用本身。
- 会话结束时，两个节点各自会为它记两行日志：字节数、重发的块数、被换掉的子连接数、卡住的时间。

## 网页界面

- 忘了密码：停掉节点，删掉数据目录里的 `ui.password`，再启动节点；新密码会在日志里显示一次。或者用 `TCPRAID_UI_PASSWORD` 或 `-ui-password-file` 设一个。
- "too many wrong passwords from this address"：等一分钟。
- 同一台主机上有两个节点时反复被登出：两者用的是同一个 cookie 名字。请给每个节点用各自的主机名来访问。
- 从别的机器打不开页面：API 监听在 `127.0.0.1`，除非用 `-api` 另外指定。

## 节点启动不了

- "the data directory ... is in use by another tcpraid"：一个目录只能有一个节点；前一个还在运行。
- "... has a state file but neither identity.key nor identity.crt: its identity is lost, and a new one is not created in its place"：从备份里恢复这些文件，或者用一个空目录启动并重新配对。
- `tcpraid status` 说 "the node is not running or -data DIR is not its data directory"：这些命令是通过数据目录找到节点的；给出和节点相同的 `-data`(在容器里是 `-data /data`)。
- 数据目录或其中的密钥文件能被其他人读或写时，日志会给出警告；请修正权限。

## 用于监控的数字

`GET /metrics` 以 Prometheus 文本格式给出计数器(需要令牌)：

```
# TYPE tcpraid_sessions_total counter
tcpraid_sessions_total 6
# TYPE tcpraid_sessions_aborted counter
tcpraid_sessions_aborted 0
# TYPE tcpraid_sessions_active gauge
tcpraid_sessions_active 1
```

没有节点应答时，`tcpraid status` 以错误状态退出；容器的健康检查用的就是它。
