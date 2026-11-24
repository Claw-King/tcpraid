# 命令参考

```
tcpraid run     [flags]                       start a node
tcpraid status  [-json]                       the node on this host
tcpraid pair    create | list | revoke ID | join [CODE]
tcpraid peers   [list] | show NAME | check NAME | remove NAME [-with-rules]
tcpraid peers   test NAME [-seconds N] [-path PATH] [-json]
tcpraid peers   set NAME [-allow LIST] [-udp-dial HOST:PORT]
tcpraid peers   path set NAME PATH [flags] | path remove NAME PATH
tcpraid rules   [list] | add NAME [flags] | remove NAME | enable NAME | disable NAME
tcpraid sessions [-json] [-watch]             what the node carries right now
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
version      0.5.0 (wire protocol 1)
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
| `pair join [CODE] [-addr HOST:PORT]` | 和生成配对码的节点配对。不带 `CODE` 时从标准输入读配对码。`-addr`：改连这个地址，而不是配对码里的地址。通往新对端的路径会记下配对码里的每一个地址，先应答的排在最前 |
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
state        online, runs 0.5.0, last heard 2026-01-15T10:00:05Z

PATH     TYPE  ADDRESS                 PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS  MARK
default  tcp   nas-a.example.org:9851  -      tls       -       -    data   8     -

$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok

$ tcpraid peers remove nas-b
peer nas-b removed. The other node is not told and still lists this one.
```

- `peers check NAME` 在每条路径上按该路径的档位打开一条连接，并让另一个节点证明它持有这个配对。路径有多个地址时，每个地址各查一次，各占一行。有路径或地址不通时以状态 1 退出，最后一行会说明是不是每条路径都还有一个能用的地址。没有通往对端路径的节点没有可检查的。
- `peers remove NAME` 在本节点上解除配对。和这个对端之间两个方向上的会话都被重置，它的连接从此被拒绝。命令随后列出仍然指向这个对端的规则。这些规则会保留，以后用同一个名字配对的节点会接手它们。带上 `-with-rules` 时，这些规则和对端一起删除。

```
$ tcpraid peers remove nas-a
peer nas-a removed. The other node is not told and still lists this one.
These rules still name nas-a: rsync, zfs
A node that is paired with this one under the same name later takes them over. To prevent that, remove them with "tcpraid rules remove".

$ tcpraid peers remove nas-a -with-rules
peer nas-a removed. The other node is not told and still lists this one.
The rules that named it were removed with it: rsync, zfs
```

### peers test

```
$ tcpraid peers test nas-a
path test with nas-a: two runs of up to 10 seconds for each path
PATH     SUB-CONNECTIONS  ONE ALONE  ALL TOGETHER  TIMES
default  8                2.6 MB/s   19.8 MB/s     7.6
default: 8 sub-connections together carried 7.6 times what one carried alone.
```

`peers test NAME` 测出子连接在每条承载数据的路径上带来了多少。节点向对端发送测试数据，对端收到后丢弃。每条路径跑两轮，第一轮只用一条数据子连接，第二轮用这条路径配置的全部条数。速度按对端报告收到的数据算，取每一轮后一半时间的平均，所以连接刚起步的那一段不计入。

- `-seconds N` 设定每一轮持续多久，取 1 到 60，默认 10。一轮发满 1 GiB 也会结束。在延迟很大的线路上，跑得久一些，数字更稳。
- `-path PATH` 只测这个名字的路径。
- `-json` 改为打印 `POST /api/v1/peers/NAME/test` 的回答(见 `reference-api.md`)。

这项测试只在你运行它的时候进行，发的是真实的数据，会和同时在跑的传输争用线路。数据从运行命令的节点发往对端。要测另一个方向，就在另一个节点上运行这条命令。没有通往对端路径的节点也能运行它，这时由对端打开子连接，表里列的是对端的路径。

全部子连接合起来的速度不到一条的 1.5 倍时，命令会这样说：

```
default: 8 sub-connections together carried little more than one alone. Either one connection already fills this line, or something on the path puts the connections into one. "Connecting two sites" in the documentation (docs/en/connecting.md) says how to tell them apart.
```

这句话的意思是，几条子连接合起来并不比一条多多少。要么一条连接已经占满了这条线路，要么路径上有东西把这几条连接合进了一条。命令分辨不出是哪一种，`connecting.md` 讲了怎么分辨。有路径没能测出来，或者测试不能开始时，命令以状态 1 退出。测试要求两个节点都是 0.3.0 或更新的版本，一个节点同一时间只跑一次测试。

### peers path

`peers path set NAME PATH [flags]` 修改对端 `NAME` 的名为 `PATH` 的路径，这个对端没有这个名字的路径时就新建一条。只有给出的项会改变。

| 参数 | 含义 | 新建路径时的取值 |
|---|---|---|
| `-addr HOST:PORT[,HOST:PORT...]` | 拨出时要连的地址：对端的监听，或者通往它的入口(端口转发、隧道)；经 SOCKS5 时是代理要去连的地址。最多 8 个地址，用逗号分隔，按这个顺序尝试(见下文) | (必须给) |
| `-type tcp\|socks5` | 直连，或者经 SOCKS5 代理 | `tcp` |
| `-proxy socks5://[user:pass@]host:port` | 代理，配合 `-type socks5` | |
| `-security tls\|owncert\|off` | 档位 | `tls` |
| `-verify pin\|ca` | `owncert` 档下：怎样核对证书 | `pin` |
| `-pin FINGERPRINT` | `owncert` 档且用 `pin` 时：证书密钥的指纹，64 位十六进制 | |
| `-roles data,repair,control` | 路径承担什么；`data` 和 `repair` 不能同时有 | `data` |
| `-subs N` | 子连接数，1 到 64 | 8 |
| `-mark N` | 这条路径的连接带的套接字标记，供主机的路由规则使用。只在 Linux 上有，节点需要 `CAP_NET_ADMIN` 权限。`0`：不带 | 不带 |

```
$ tcpraid peers path set nas-a side -addr 192.0.2.10:9851 -type socks5 -proxy socks5://user:pass@127.0.0.1:1080 -roles repair,control -subs 2
path side of nas-a added
PATH     TYPE    ADDRESS                 PROXY                    SECURITY  VERIFY  PIN  ROLES           SUBS  MARK
default  tcp     nas-a.example.org:9851  -                        tls       -       -    data            8     -
side     socks5  192.0.2.10:9851         socks5://127.0.0.1:1080  tls       -       -    repair,control  2     -

$ tcpraid peers path remove nas-a side
path side of nas-a removed
```

一个对端最多有 16 条路径。路径的名字最长 32 字节，不能有空格。不合规定的路径会被拒绝，什么都不会改变。显示代理时不带它的用户名和密码。

节点对这一条路径的修改是一步完成的。两条这样的命令同时运行、改的是同一个对端的两条路径时，两处修改都生效。同时运行的 `peers set` 也一样。

**多个地址。** 一条路径可以写同一个监听的最多 8 个地址，比如先写它的 IPv6 地址，再写一个转发出来的 IPv4 端口。节点每次打开连接时按给出的顺序尝试，每个地址最多等 8 秒。它会记住哪个地址连通了，下次从那个地址开始，直到节点重启。`-addr` 每次设置的都是整个列表，所以只给一个地址就会去掉其余的。

```
$ tcpraid peers path set nas-a default -addr [2001:db8::10]:9851,203.0.113.7:9851
path default of nas-a changed
PATH     TYPE  ADDRESS                               PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS  MARK
default  tcp   [2001:db8::10]:9851,203.0.113.7:9851  -      tls       -       -    data   8     -

$ tcpraid peers check nas-a
default  [2001:db8::10]:9851 (tls): FAILED: dial tcp [2001:db8::10]:9851: connect: network is unreachable
default  203.0.113.7:9851 (tls): ok
tcpraid peers check: 1 of 2 address(es) to nas-a failed. Every path still has an address that works.
```

**标记。** 带标记的路径会给它打开的每条连接设上这个标记，这样主机的路由规则可以让这些连接走另一个出口。设置标记需要 `CAP_NET_ADMIN` 权限，从 Linux 5.17 起有 `CAP_NET_RAW` 也可以。没有权限时，这条路径的连接会失败，`peers check` 会说明缺的是这项权限。在 macOS 和 Windows 上，设置标记时会被拒绝。

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

- `-listen IP:PORT[-PORT] -dest HOST:PORT[-PORT]`：固定规则。`-listen` 的 IP 可以不写，例如 `:9801`，表示所有地址。IPv6 地址要放在方括号里。两段端口长度相同，最多 1024 个端口，逐个端口对应。两条规则不能监听同一个地址的同一个端口。目的地可以写 IP 地址，也可以写主机名，例如 `-dest nas.lan:873`。目的地在对端的网络里，所以名字由对端解析，每条连接解析一次，连接去往对端给本节点设的允许列表放行的第一个地址。写主机名时，两个节点都要是 0.4.0 或更新的版本。
- `-match NETWORKS`：透明规则，用于以 `-transparent` 启动的节点。网段写成 CIDR 形式，可带端口或端口段，用逗号分隔，例如 `192.0.2.0/24:873,[2001:db8::/32]:5000-5010`。

`-disabled` 把规则以停用状态保存。一个节点最多有 256 条规则，规则的名字最长 64 字节，不能有空格。

另有两个参数用来限定规则的连接用什么：

| 参数 | 含义 |
|---|---|
| `-paths NAME[,NAME...]` | 规则的连接只用这几条路径，它们必须是两个节点之间承载数据的路径。不给时用全部路径 |
| `-subs N` | 规则的每条连接在每条路径上最多保持 `N` 条数据子连接，并且不超过路径自己设的条数。不给时用路径设的条数 |

路径的名字以持有路径的节点起的为准。在持有路径的节点上，`tcpraid peers show` 会列出它们，添加规则时写了不存在的名字会被拒绝。在由对端持有路径的节点上，名字由对端在连接到来时核对，而且两个节点都要是 0.5.0 或更新的版本。控制和补发用的子连接不受这两个参数影响。被限定的规则会在状态里写明：

```
$ tcpraid rules add pull -peer nas-a -listen 0.0.0.0:9802 -dest 192.0.2.10:873 -paths side -subs 3
rule pull added: listening, over the paths side, with at most 3 sub-connections on a path
```

`rules enable` 和 `rules disable` 会保留这两项设置。要修改它们，就删掉规则再重新添加。

`STATE` 是 `listening`、`disabled`，或者 `FAILED:` 加上规则没能监听的原因。这时 `rules add` 以状态 1 退出，规则已经保存，规则有变动时会再试。"last connection:" 后面是这条规则最近一条没能承载的连接出了什么问题，以及那是多久以前：

```
$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:9801  nas-a  192.0.2.10:873  0         listening, last connection: peer "nas-a" could not connect to 192.0.2.10:873: connect: connection refused (12s ago)
```

对端连不上目的地，或者对端的允许列表里没有这个目的地时，对端会把原因报回来，规则把它显示出来。0.3.0 之前的对端不会报，这时规则会让你去看对端的日志。连接因为对端不应答而等着的时候，规则会说明那些尝试碰到了什么，这种情况不带时间。这条规则的下一条连接到来时，上一条留下的内容会被清掉。

两个节点各自持有通往对方的路径、而本节点自己的路径不通时，新会话的连接改由对端打开，直到本节点的路径恢复(见 `connecting.md`)。这期间规则的状态是 `listening, the peer opens the connections while this node's path is down`。带 `-paths` 或 `-subs` 的规则不会这样切换，因为它写的名字是本节点的路径。

目的地是主机名的规则，在名字没有解析出地址时也会写明原因，比如对端不认识这个名字、对端没有连上、对端的允许列表里没有这个名字的任何地址，或者对端的版本不解析名字。

## sessions

```
$ tcpraid sessions
ID      ROLE    RULE   PEER   DESTINATION     AGE  SENT      RECEIVED  AVERAGE    NOW        SENT AGAIN  SUBS  NOT SENT YET  IN FLIGHT  STATE
4a5411  client  rsync  nas-a  192.0.2.10:873  6s   320.0 MB  0 B       53.3 MB/s  30.0 MB/s  0.0%        8     96.0 MB       28.0 MB    ok
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
| `NOT SENT YET` | 已从应用读进来、还在等子连接的数据。这个数一直很大，说明应用给数据的速度比线路能带的快 |
| `IN FLIGHT` | 已经发出、对端还没有报告收到的数据 |
| `STATE` | `ok`；对端还没有在任何子连接上应答时是 `connecting`；或者会话已经等了对端多久 |

`SENT` 算的是已经发往对端的数据。其中一部分可能还在路上，所以它会比另一端显示的 `RECEIVED` 多。这条命令说不出一次传输还要多久，因为 tcpraid 不知道应用总共要发多少。

两个节点之间的线路断了的时候，会话会保持一段时间(`-grace`，默认 15 分钟)，线路恢复后接着传。这时 `STATE` 显示已经多久没有从对端收到东西，以及保持时间还剩多少：

```
ID      ROLE    RULE   PEER   DESTINATION     AGE    SENT     RECEIVED  AVERAGE   NOW       SENT AGAIN  SUBS  NOT SENT YET  IN FLIGHT  STATE
4a5411  client  rsync  nas-a  192.0.2.10:873  5m12s  2.10 GB  0 B       6.9 MB/s  0.0 MB/s  1.0%        0     104.0 MB      24.0 MB    waiting for the peer for 3m12s, gives up in 11m48s
```

时间到了，会话结束，应用的连接被重置。路径测试的会话在 `RULE` 和 `DESTINATION` 两栏显示 `(path test)`。

`-watch` 每 2 秒把表格再打印一次，每次上面带一行当时的时间，直到你中断它。`-interval DURATION` 可以换一个间隔，从 `1s` 到 `1h`。

`-json` 改为打印 `GET /api/v1/sessions` 的回答，只取一次(见 `reference-api.md`)。

## version

```
$ tcpraid version
tcpraid 0.5.0 (wire protocol 1)
```
