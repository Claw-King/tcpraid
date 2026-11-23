# 命令参考

```
tcpraid run     [flags]                       start a node
tcpraid status  [-json]                       the node on this host
tcpraid pair    create | list | revoke ID | join [CODE]
tcpraid peers   [list] | show NAME | check NAME | remove NAME
tcpraid peers   set NAME [-allow LIST] [-udp-dial HOST:PORT]
tcpraid peers   path set NAME PATH [flags] | path remove NAME PATH
tcpraid rules   [list] | add NAME [flags] | remove NAME | enable NAME | disable NAME
tcpraid sessions [-json]                      what the node carries right now
tcpraid version
```

第一个参数不以 `-` 开头时，它是一个命令名。其他形式的命令行都是手动模式(见 `manual-mode.md`)。`tcpraid COMMAND -h` 列出该命令的参数。

除了 `run` 和 `version`，每条命令都是和同一台主机上的节点通信，并从节点的数据目录里读出 API 的地址和令牌。所以它们都接受 `-data DIR`，并且必须由能读这个目录的用户来执行。默认值在 Linux 上是 `/var/lib/tcpraid`，在 macOS 和 Windows 上是当前用户配置目录下的 `tcpraid`，在容器镜像里是 `/data`。在容器里，命令是 `docker exec tcpraid /tcpraid COMMAND ... -data /data`。

## run

启动一个节点。它的参数见 `reference-files.md`，包括在哪里监听，以及传输层的六项设置(`-window`、`-budget`、`-grace`、`-hol`、`-timeline`、`-maxrss`)。收到 SIGINT 或 SIGTERM 时，它先通知各个对端、重置自己承载的应用连接，然后以退出状态 0 停止。

## status

```
$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.2.0 (wire protocol 1)
uptime       1s
sessions     0
peers        0
rules        0
```

`-json` 改为输出 API 的回答。没有节点应答时，它会说明这一点，并以状态 1 退出。

## pair

| 命令 | 作用 |
|---|---|
| `pair create [-addr HOST:PORT]... [-ttl DURATION]` | 生成一个配对码并输出。`-addr`：另一个节点用什么地址连到本节点(默认：节点的 `-advertise`)；可以给多次，最多 8 个。`-ttl`：有效期多长(默认 `1h`，最长 30 天) |
| `pair join [CODE] [-addr HOST:PORT]` | 和生成配对码的节点配对。不带 `CODE` 时从标准输入读配对码。`-addr`：改连这个地址，而不是配对码里的地址 |
| `pair list` | 尚未过期的配对码 |
| `pair revoke ID` | 让一个配对码作废 |

```
$ tcpraid pair create
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node with "tcpraid pair join", which then reads the code from standard input.
It is shown this once and is a secret until it is used.
tcpraid-pair-1-ueazax3dhku3...

$ tcpraid pair list
ID        VALID UNTIL           USED
a101905f  2026-01-15T11:00:00Z  no

$ tcpraid pair revoke a101905f
pairing code a101905f revoked
```

只有配对码本身输出到标准输出。关于它的几行说明输出到标准错误，所以脚本可以用 `CODE=$(tcpraid pair create)` 单独取到配对码。

作为参数交给 `pair join` 的配对码会出现在进程列表里，并留在 shell 的历史里。在配对码仍然有效期间，这一点是要紧的。

## peers

```
$ tcpraid peers
NAME   STATE   FINGERPRINT                                                       PAIRED                PATHS
nas-a  online  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86  2026-01-15T10:00:00Z  nas-a.example.org:9851 (tls, data)

$ tcpraid peers show nas-a
name         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
paired       2026-01-15T10:00:00Z
addresses    nas-a.example.org:9851
state        online, runs 0.2.0, last heard 2026-01-15T10:00:05Z

PATH     TYPE  ADDRESS                 PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS
default  tcp   nas-a.example.org:9851  -      tls       -       -    data   8

$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok

$ tcpraid peers remove nas-b
peer nas-b removed. The other node is not told and still lists this one.
```

- `peers check NAME` 在每条路径上按该路径的档位打开一条连接，并让另一个节点证明它持有这个配对。有路径不通时以状态 1 退出。没有通往对端路径的节点没有可检查的。
- `peers remove NAME` 在本节点上解除配对。和这个对端之间两个方向上的会话都被重置，它的连接从此被拒绝。

### peers path

`peers path set NAME PATH [flags]` 修改对端 `NAME` 的名为 `PATH` 的路径，这个对端没有这个名字的路径时就新建一条。只有给出的项会改变。

| 参数 | 含义 | 新建路径时的取值 |
|---|---|---|
| `-addr HOST:PORT` | 拨出时要连的地址：对端的监听，或者通往它的入口(端口转发、隧道)；经 SOCKS5 时是代理要去连的地址 | (必须给) |
| `-type tcp\|socks5` | 直连，或者经 SOCKS5 代理 | `tcp` |
| `-proxy socks5://[user:pass@]host:port` | 代理，配合 `-type socks5` | |
| `-security tls\|owncert\|off` | 档位 | `tls` |
| `-verify pin\|ca` | `owncert` 档下：怎样核对证书 | `pin` |
| `-pin FINGERPRINT` | `owncert` 档且用 `pin` 时：证书密钥的指纹，64 位十六进制 | |
| `-roles data,repair,control` | 路径承担什么；`data` 和 `repair` 不能同时有 | `data` |
| `-subs N` | 子连接数，1 到 64 | 8 |

```
$ tcpraid peers path set nas-a side -addr 192.0.2.10:9851 -type socks5 -proxy socks5://user:pass@127.0.0.1:1080 -roles repair,control -subs 2
path side of nas-a added
PATH     TYPE    ADDRESS                 PROXY                    SECURITY  VERIFY  PIN  ROLES           SUBS
default  tcp     nas-a.example.org:9851  -                        tls       -       -    data            8
side     socks5  192.0.2.10:9851         socks5://127.0.0.1:1080  tls       -       -    repair,control  2

$ tcpraid peers path remove nas-a side
path side of nas-a removed
```

一个对端最多有 16 条路径。路径的名字最长 32 字节，不能有空格。不合规定的路径会被拒绝，什么都不会改变。显示代理时不带它的用户名和密码。

### peers set

`peers set NAME [-allow LIST] [-udp-dial HOST:PORT]` 修改对端 `NAME` 的两项设置。只有给出的项会改变，给空值就是清除这项设置。两个参数都不给时，命令什么也不改并报错(退出状态 1)。

| 参数 | 含义 |
|---|---|
| `-allow LIST` | 这个对端经过本节点可以访问的目的地：若干网段，可带端口或端口段，用逗号分隔。`""`：任何目的地。要在对端的连接出来的节点上设(见 `security.md`) |
| `-udp-dial HOST:PORT` | 本节点把和这个对端之间会话的 UDP 回执发到哪里：对端 `-udp-listen` 的地址。`""`：不发(见 `concepts.md`) |

```
$ tcpraid peers set nas-a -allow 192.0.2.10/32:873,192.0.2.10/32:50000-50010
nas-a may reach through this node: 192.0.2.10/32:873,192.0.2.10/32:50000-50010

$ tcpraid peers set nas-a -allow ""
nas-a may reach any destination through this node

$ tcpraid peers set nas-b -udp-dial 192.0.2.20:8446
UDP feedback for sessions with nas-b goes to 192.0.2.20:8446
```

这两项设置了的时候，`peers show NAME` 会把它们列出来。在网页界面里，它们在 对端 > 详情 > 高级 下面。

## rules

```
$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:9801 -dest 192.0.2.10:873
rule rsync added: listening

$ tcpraid rules add zfs -peer nas-a -listen 0.0.0.0:50000-50002 -dest 192.0.2.10:50100-50102
rule zfs added: listening

$ tcpraid rules
NAME   TAKES                PEER   DESTINATION             SESSIONS  STATE
rsync  0.0.0.0:9801         nas-a  192.0.2.10:873          0         listening
zfs    0.0.0.0:50000-50002  nas-a  192.0.2.10:50100-50102  3         listening

$ tcpraid rules disable zfs
rule zfs: disabled

$ tcpraid rules enable zfs
rule zfs: listening

$ tcpraid rules remove rsync
rule rsync removed. Connections it carries now run to their end.
```

`rules add NAME -peer PEER`，后面跟下面两者之一：

- `-listen IP:PORT[-PORT] -dest IP:PORT[-PORT]`：固定规则。`-listen` 的 IP 可以不写，例如 `:9801`，表示所有地址。IPv6 地址要放在方括号里。两段端口长度相同，最多 1024 个端口，逐个端口对应。目的地是 IP 地址，不支持主机名。两条规则不能监听同一个地址的同一个端口。
- `-match NETWORKS`：透明规则，用于以 `-transparent` 启动的节点。网段写成 CIDR 形式，可带端口或端口段，用逗号分隔，例如 `192.0.2.0/24:873,[2001:db8::/32]:5000-5010`。

`-disabled` 把规则以停用状态保存。一个节点最多有 256 条规则，规则的名字最长 64 字节，不能有空格。

`STATE` 是 `listening`、`disabled`，或者 `FAILED:` 加上规则没能监听的原因。这时 `rules add` 以状态 1 退出，规则已经保存，规则有变动时会再试。"last connection:" 后面是这条规则最近一条没能承载的连接出了什么问题。

## sessions

```
$ tcpraid sessions
ID      ROLE    RULE   PEER   DESTINATION     AGE  SENT      RECEIVED  AVERAGE    NOW        SENT AGAIN  SUBS
4a5411  client  rsync  nas-a  192.0.2.10:873  6s   320.0 MB  0 B       53.3 MB/s  30.0 MB/s  0.0%        8
```

节点此刻承载的每一条应用连接一行，没有时显示 `no sessions`。这条命令会隔一秒看两次，以便算出速度。

| 列 | 含义 |
|---|---|
| `ID` | 会话号的前六位，和节点日志里的一样 |
| `ROLE` | `client`：这条连接是本节点的某条规则接下的。`server`：本节点是出口，连接是对端的规则接下的 |
| `RULE`、`PEER`、`DESTINATION` | 规则(在 `client` 这一端)、另一端的对端、连接去往哪里 |
| `AGE` | 会话已经运行了多久 |
| `SENT`、`RECEIVED` | 发给对端和从对端收到的流数据 |
| `AVERAGE`、`NOW` | 两个方向合计的速度：从会话开始算起的，和最近一秒的 |
| `SENT AGAIN` | 被排进重发队列的数据，占已发数据的比例 |
| `SUBS` | 存活的子连接数 |

`SENT` 算的是已经发往对端的数据。其中一部分可能还在路上，所以它会比另一端显示的 `RECEIVED` 多。这条命令说不出一次传输还要多久，因为 tcpraid 不知道应用总共要发多少。

`-json` 改为打印 `GET /api/v1/sessions` 的回答，只取一次(见 `reference-api.md`)。

## version

```
$ tcpraid version
tcpraid 0.2.0 (wire protocol 1)
```
