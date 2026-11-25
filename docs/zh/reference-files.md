# 节点的选项和文件

## `tcpraid run` 的选项

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-data DIR` | Linux 上是 `/var/lib/tcpraid`；macOS 和 Windows 见“数据目录”一节 | 数据目录；不存在时会创建(权限 0700) |
| `-api ADDR` | `127.0.0.1:9850` | 本机 API 和网页界面的监听地址(明文 HTTP，带 `-api-tls` 时是 HTTPS) |
| `-api-tls` | 关闭 | 经 TLS 提供本机 API 和网页界面，用的是节点自己生成并存在数据目录里的证书。本机上的命令会自己跟上(见 `security.md`) |
| `-api-cert FILE`、`-api-key FILE` | 无 | 一张证书和它的密钥(PEM)，用来经 TLS 提供本机 API 和网页界面，取代节点自己的那张 |
| `-name NAME` | 它已有的名字 | 节点的名字。给了就给节点改名。不给，节点保留原来的名字。第一次启动时用主机名加上一个连字符和四个随机字符，例如 `nas-k3x9`，这样主机名相同的两台机器不会变成两个同名的节点。名字是 1 到 64 字节的可打印字符，两端不能有空格 |
| `-listen ADDR` | 关闭 | 供其他节点连接的监听地址(TLS)。没有它，节点可以去加入别的节点，但别的节点不能来加入它 |
| `-advertise HOST:PORT` | 无 | 其他节点连 `-listen` 时用的地址；会写进本节点生成的配对码。可以给多次 |
| `-listen-plain ADDR` | 关闭 | 不带 TLS 的对端监听地址，给 `off` 档的路径用 |
| `-cert FILE`、`-key FILE` | 无 | 一张证书和它的密钥(PEM)，给使用 `owncert` 档的对端；需要同时有 `-listen`。节点启动时会在日志里记下证书密钥的指纹 |
| `-transparent ADDR` | 关闭 | 接收由主机重定向到节点的连接的监听地址；它们去哪里由透明规则决定。只在 Linux 上有 |
| `-redirect MODE` | 关闭 | 节点用主机上的 `nft` 程序为自己的透明规则安装重定向规则，停止时再删掉。`MODE` 是 `forwarded`、`local` 或 `both`。只在 Linux 上有，要和 `-transparent` 一起用。见“节点自己安装的重定向规则” |
| `-redirect-print` | 关闭 | 和 `-redirect` 一起用：节点在启动时和规则变化时把它本来要交给 `nft` 的规则写进日志，一条也不安装 |
| `-udp-listen ADDR` | 关闭 | 接收对端回执的监听地址(UDP)，对端要被告知把回执发到这里 |
| `-ui-password-file FILE` | 无 | 存放网页界面密码的文件 |
| `-no-ui` | 关闭 | 关掉网页界面。它的文件和登录接口回答 `404`，也不生成密码。带令牌的 API 照常可用 |
| `-spares N` | `2` | 备用连接的条数，0 到 8。节点向它没有路径可拨的对端要这么多条，并且最多给来要的对端保持这么多条。`0` 是关掉(见 `connecting.md` 的“备用连接”) |
| `-metrics ADDR` | 关闭 | 节点各项数字的监听地址：`/metrics` 是 Prometheus 文本格式，`/metrics.json` 是 JSON，只读，不需要令牌(见“各项数字和钩子”) |
| `-hook COMMAND` | 无 | 一个程序。对端上线或下线、会话开始或结束、规则的连接失败时，节点运行它(见“各项数字和钩子”) |
| `-hook-timeout DURATION` | `30s` | `-hook` 的一次运行最长多久，超过就被节点结束。`1s` 到 `1h` |
| `-stats DURATION` | 关闭 | 每隔这么久在日志里记一行节点的合计数字，并为当时在跑的每个会话各记一行(这些行的含义见 `troubleshooting.md`) |
| `-window SIZE` | `128MB` | 每个会话：本节点最多持有多少“对端还没报告收到”的数据，以及它为乱序到达的数据向对端提供的空间。整数 MB |
| `-budget SIZE` | `256MB` | 所有会话合计：最多持有多少未确认的数据，另外还能持有同样多的乱序数据 |
| `-grace DURATION` | `900s` | 对端没有任何消息到达时，会话保持多久 |
| `-hol DURATION` | `1s` | 后面的数据等一个缺失的块，超过这个时间加上允许的排队时间(`-target`，报告中断过之后的一段时间里是 `-pigtarget`)，就单独重发它。同一个块每多重发一次，再等同样长的时间 |
| `-target DURATION` | `300ms` | 一条子连接允许的排队时间：除了它的回执往返一圈之内到得了的数据之外，它在路上还可以压多少数据，按它的送达速率折算成时间。`20ms` 到 `5s` |
| `-pigtarget DURATION` | `1s` | 对端的报告中断超过 1 秒之后的 10 秒里用的允许排队时间。`20ms` 到 `30s` |
| `-dead DURATION` | `20s` | 一条子连接路上有数据却这么久没有进展，或者这么久写不进去，就被关掉并换一条。`5s` 到 `10m` |
| `-kickratio NUMBER` | `5` | 一条忙着的子连接，送达速率连续 10 秒低于它所在路径的中位数除以这个数，就被换掉。`1.5` 到 `100` |
| `-kickfloor SIZE` | `256KB` | 路径的中位速率低于每秒这么多字节时，不因为慢而换掉任何子连接。`1KB` 到 `1GB` |
| `-share` | 开 | 每条子连接只占窗口里按它的送达速率分给它的部分。`-share=false` 关掉这一条 |
| `-fbsubs N` | `3` | 会话既没有控制子连接、也没有 UDP 回执时，每份“收到了什么”的报告发在这么多条数据子连接上。`1` 到 `16` |
| `-fbevery DURATION` | `20ms` | 有变化时每隔多久发一份“收到了什么”的报告。没有变化时也每秒发一份。`5ms` 到 `1s` |
| `-coalesce DURATION` | `20ms` | 一个块没装满时，节点为等应用送来更多数据最多等多久再发。`0s` 到 `1s` |
| `-timeline DURATION` | 关闭 | 每隔这么久为每个会话在日志里记一行数字(这一行的含义见 `troubleshooting.md`) |
| `-maxrss MB` | 关闭 | 节点的常驻内存超过这么多 MB 时停止节点。只在 Linux 上有 |

日志的启动行会把实际生效的设置重复一遍：

```
node 0.10.0 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
transport window=128MB budget=256MB grace=20m0s hol=1s timeline=off maxrss=off
tuned target=500ms
```

`transport` 这一行永远是这六项。从 `-target` 到 `-coalesce` 的设置里有不是默认值的，才会有 `tuned` 这一行，它只列出那几项。节点里存着运行中改过的设置时，日志里还有第三行把它们列出来。

### 传输层的设置

从 `-window` 到 `-maxrss` 的选项改变本节点传输层的工作方式。默认值适合大多数路径，有测量结果支持时再改。节点为每个会话记在日志里的数字怎么读，见 `troubleshooting.md`。本节末尾的表说明哪一项设置对应哪一个数字。

每一项设置只管会话在本节点这一端的行为，不会发给对端。只有一个例外。一个节点的 `-window` 是它发送时用的窗口，也是它接收时提供的空间，对端在会话开始时得知它。一对节点的两端可以用不同的值。

一个会话的每一项设置按下面的顺序从三个地方取：

| 层 | 怎么设 | 对什么起作用 |
|---|---|---|
| 规则 | `tcpraid rules add NAME ... -SETTING VALUE`、`tcpraid rules set NAME -SETTING VALUE` | 这条规则的连接 |
| 对端 | `tcpraid peers set NAME -SETTING VALUE` | 和这个对端之间的会话，不管规则在两个节点的哪一个上 |
| 节点 | 启动时 `tcpraid run -SETTING VALUE`，运行中 `tcpraid transport set -SETTING VALUE` | 节点的每一个会话 |

规则没有设的取对端的，对端没有设的取节点的。规则上的设置在规则所在的节点上起作用。同一个会话的另一个节点用它自己的设置，也就是它为对端存的设置和它节点的设置。`-budget` 和 `-timeline` 属于一个节点的全部会话，只有节点这一层。`-maxrss` 和 `-stats` 只能在节点启动时设。

会话在开始时取得它的设置，一直用到结束。三个地方的任何一处改动，都只对之后开始的会话起作用。`tcpraid transport`、`tcpraid peers set` 和 `tcpraid rules set` 见 `reference-cli.md`。

`tcpraid run` 的命令行上给出的设置在这个节点运行期间一直有效，`tcpraid transport set` 不能改它。用 `tcpraid transport set` 改的设置存在 `state.json` 里，重启之后仍然有效，除非下一次启动的命令行给了同一项设置。

从 0.8.0 版起，网页界面在“设置”页里显示节点的这些设置和每个值的来历，并且可以在那里修改，效果和 `tcpraid transport set` 一样。命令行上给出的设置在页面里同样改不了。对端和规则的设置显示在对端和规则那里，要用命令来改。在网页界面里改一个对端或一条规则，它的传输层设置保持原样。

- 大小写成 `256MB`、`64KB`、`1GB`，或者一个字节数。时间写成 `3600s`、`500ms`、`15m`。
- `-window` 是整数 MB，至少 1 MB。`-budget` 至少 1 MB。`-grace` 和 `-hol` 大于零。其余各项的范围在上面的表里。
- `-maxrss` 不能低于 2 × `-budget` + 100 MB，因为节点在正常运行中最多会持有两倍 `-budget` 的流数据。超过上限时，节点像收到信号那样停止。它通知对端，重置它承载的应用连接，以状态 1 退出，并在日志里写明原因。配了重启策略的容器随后会把它重新启动，但会话不会恢复。这个选项还会把 Go 运行时的软性内存上限设为该值的 70%。运行中存下的 `budget` 和后来某次启动的 `-maxrss` 对不上时，节点不会启动。报错会说明这一点，命令行上的 `-budget` 优先于存下的值。
- 不能用的值会让 `tcpraid run` 立刻结束，此时它还没有监听任何地址，报错会点名是哪个选项。
- 块大小(手动模式的 `-chunk`)不是节点的设置。每个节点都用 256 KB，因为一个会话的两端应该用同一个值。

每一项设置改的是什么，以及日志里和它对应的数字：

| 设置 | 值调大 | 什么时候看它 |
|---|---|---|
| `-window` | 节点可以持有更多“已发出、对端还没报告收到”的数据，并给对端更多空间放乱序到达的数据 | `pumpwait` 接近 1 而 `unsent` 接近 0(见 `troubleshooting.md`) |
| `-budget` | 所有会话合起来可以持有更多 | 同时跑着几个会话，它们的窗口加起来超过了 `budget` |
| `-grace` | 对端没有消息时，会话保持得更久 | 线路一次会断 15 分钟以上而不希望应用被切断，或者希望会话早一点放弃 |
| `-hol` | 缺失的块等得更久才重发 | 接收端的 `holage` 一直大于零。发送端的 `-hol` 调短会更早重发这个块，代价是有些块会发两遍 |
| `-target`、`-pigtarget` | 每条子连接可以在路上压更多数据，一条子连接卡住时排在它后面的也更多 | `cap=A/N` 显示大多数子连接顶在上限，或者结束行里的 `peer wait` 很长，因为这个等待就是 `-hol` 加上它 |
| `-dead` | 没有进展的子连接留得更久 | 结束行里有子连接以 `silent` 或 `write` 结束，而这条路径本来就会停顿很久再继续 |
| `-kickratio` | 因为慢而被换掉的子连接更少 | 结束行里的 `kicks` 很多，而这条路径上各条子连接的速度一直就不一样 |
| `-kickfloor` | 路径速度更高时才开始因为慢而换子连接 | 同上，路径本身很慢时 |
| `-share` | (开关)关掉之后，慢的子连接可以和快的占一样多的窗口 | 想对比两种做法时 |
| `-fbsubs` | 每份报告发在更多条数据子连接上 | 没有哪条路径有控制角色，也没有配 UDP 回执，而结束行里的 `fb gap max` 很长 |
| `-fbevery` | 报告发得更少，发送端更晚才知道什么到了 | 回程方向很慢，报告占了它可观的一部分 |
| `-coalesce` | 为了装满一个块等得更久 | 应用每次只发一小段并等对方回答。`0s` 是每一小段都不等，立刻发 |

### 节点自己安装的重定向规则

透明规则接的是主机重定向到节点透明监听上的连接。不带 `-redirect` 时，主机上的重定向规则由你自己写(见 `install-binary.md`)。带 `-redirect` 时，节点为已启用的透明规则写出重定向规则，并在这些规则变化时跟着改。0.9.0 版新增：

```
# tcpraid run -transparent :9800 -redirect forwarded ...
# tcpraid rules add lan -peer nas-b -match 198.51.100.0/24:5000
# nft list table inet tcpraid
table inet tcpraid {
	chain prerouting {
		type nat hook prerouting priority dstnat - 10; policy accept;
		ip daddr 198.51.100.0/24 tcp dport 5000 redirect to :9800
	}
}
```

| `MODE` | 被重定向的连接 | 链 |
|---|---|---|
| `forwarded` | 经过本机转发的连接，路由器上就是这种 | `prerouting` 钩子 |
| `local` | 本机上的程序自己发起的连接 | `output` 钩子 |
| `both` | 两种都有 | 两条链 |

- **只动一张表。** 节点写的是 `inet` 族里名叫 `tcpraid` 的表，不读也不改别的表。启动时如果已经有这个名字的表，就把它换掉。停止时把表删掉。被强行杀掉的节点做不到这一步，表会留下来，它重定向的连接找不到监听，直到节点再次运行，或者你用 `nft delete table inet tcpraid` 删掉这张表。
- **节点的链排在哪里。** 它们的优先级是 -110。`nft` 的手册说，同一个钩子上的链按优先级从小到大依次经过，并给出目的地址转换(`dstnat`)的标准优先级是 -100，所以节点的链排在处于这个标准优先级的链之前。
- **监听要在连接到达的地方。** 规则用的是 `redirect` 语句，`nft` 的手册说它是目的地址转换的一种形式，"always translates the destination address to the local host's one"。监听所有地址的透明监听，例如 `-transparent :9800`，收得到这些连接。
- **节点自己的连接。** 用 `local` 和 `both` 时，`output` 链的第一条规则放过和节点同一个用户运行的进程的所有连接。没有它，节点去连目的地的连接可能被重定向回节点自己。所以用同一个用户运行的程序也不会被重定向。要重定向本机上的程序时，让节点用单独的用户运行，`deploy/` 里的 systemd 单元就是这样做的。
- **需要什么。** Linux，节点的 `PATH` 里有 `nft` 程序，以及 `CAP_NET_ADMIN` 权限。缺任何一样，带 `-redirect` 启动的节点会退出并说明缺的是什么。容器镜像里没有 `nft`。`deploy/` 里的 systemd 单元不给节点任何权限，所以配合它用 `-redirect` 之前，要在单元里那两行权限设置中加上 `CAP_NET_ADMIN`。
- `-redirect-print` 显示节点本来要交给 `nft` 的内容，什么都不安装，既不需要 `nft` 也不需要这项权限。

### 各项数字和钩子

**`-metrics ADDR`** 在一个单独的地址上提供节点的各项数字。这个地址只回答两种请求：`GET /metrics` 是 Prometheus 文本格式，`GET /metrics.json` 是同样的样本，写成 JSON。它不要令牌。API 自己的 `/metrics` 照旧存在，和 API 的每个路径一样需要令牌。

```
$ curl -s http://127.0.0.1:9900/metrics
# TYPE tcpraid_sessions_total counter
tcpraid_sessions_total 1
# TYPE tcpraid_bytes_tx counter
tcpraid_bytes_tx 50331648
...
# TYPE tcpraid_peer_online gauge
tcpraid_peer_online{peer="nas-b"} 1
# TYPE tcpraid_rule_listening gauge
tcpraid_rule_listening{rule="zfs"} 1
# TYPE tcpraid_rule_sessions gauge
tcpraid_rule_sessions{rule="zfs"} 0
```

| 数字 | 含义 |
|---|---|
| `tcpraid_sessions_total`、`tcpraid_sessions_aborted`、`tcpraid_sessions_active` | 建过的会话数、被切断的会话数、现在在跑的会话数 |
| `tcpraid_subs_up`、`tcpraid_subs_lost` | 建起来的子连接数、丢掉的子连接数 |
| `tcpraid_kicks_slow`、`tcpraid_kicks_silent`、`tcpraid_kicks_write` | 因为慢、因为没有进展、因为写不进去而被换掉的子连接数 |
| `tcpraid_bytes_tx`、`tcpraid_chunks_tx`、`tcpraid_resend_chunks`、`tcpraid_resend_by{cause=...}` | 第一次发出的数据，按字节和按块，以及排队重发的块数，总数和按原因分的 |
| `tcpraid_unacked_mb`、`tcpraid_rss_mb`、`tcpraid_runtime_memory_mb` | 所有会话还没被确认的数据、系统算出的进程内存(只在 Linux 上有)、程序从系统拿到的内存 |
| `tcpraid_peer_online{peer=...}` | 和这个对端之间有控制连接时是 1，否则是 0 |
| `tcpraid_rule_listening{rule=...}`、`tcpraid_rule_sessions{rule=...}` | 规则在接连接时是 1，以及它现在承载的会话数 |
| `tcpraid_hook_running`、`tcpraid_hook_runs`、`tcpraid_hook_events_dropped`、`tcpraid_hook_runs_failed`、`tcpraid_hook_runs_timed_out` | 只在带 `-hook` 时有：正在进行的运行数、启动过的运行数、没有交给钩子的事件数、失败的运行数、被结束的运行数 |

其余的是传输层另外的一些计数器。

这些都是数字。对端和规则的名字在里面，作为标签 `peer` 和 `rule` 的值。里面没有地址，没有目的地，没有密钥和令牌，也没有失败原因的文字。能连到这个地址的人都能读到它们，所以光是这些名字就嫌多的地方，不要开这个选项。节点同时回答四个取数字的请求，第五个会被告知稍后再来，而请求不会改变节点的任何东西。地址不是回环地址时，节点在启动时的日志里说明这个地址上的数字不需要令牌。

**`-hook COMMAND`** 指定一个程序，有事发生时节点运行它。`COMMAND` 就是程序本身，可以是一个路径，也可以是一个要在 `PATH` 里找的名字。它被直接执行，不经过 shell，也不带参数。程序不能运行时，节点在启动时就停下。发生的事用两种方式交给程序：环境变量，以及标准输入上的一行 JSON。

| 事件(`TCPRAID_EVENT`) | 什么时候 | 带的字段 |
|---|---|---|
| `peer_online` | 和一个对端之间的第一条控制连接建起来了 | peer |
| `peer_offline` | 和一个对端之间的最后一条控制连接没有了 | peer |
| `session_start` | 本节点建了一个会话 | peer、rule、session、role、destination |
| `session_end` | 本节点的一个会话结束了 | 同上，另有 tx_bytes、rx_bytes、clean、reason |
| `rule_failure` | 规则的一条连接、或者 `tcpraid connect` 的一条连接没有被承载 | rule、reason |

| 变量 | JSON 字段 | 内容 |
|---|---|---|
| `TCPRAID_EVENT` | `event` | 事件 |
| `TCPRAID_TIME` | `time` | 发生的时间，RFC 3339 |
| `TCPRAID_PEER` | `peer` | 对端的名字 |
| `TCPRAID_RULE` | `rule` | 规则的名字。对端带来的会话和 `tcpraid connect` 没有这一项 |
| `TCPRAID_SESSION` | `session` | 会话的编号，32 位十六进制。`tcpraid sessions` 显示它的前六位 |
| `TCPRAID_ROLE` | `role` | 本节点接了应用的连接时是 `client`，会话是对端带来的时是 `server` |
| `TCPRAID_DESTINATION` | `destination` | 连接在对面节点那边去哪里 |
| `TCPRAID_TX_BYTES`、`TCPRAID_RX_BYTES` | `tx_bytes`、`rx_bytes` | 本节点发出和收到的数据 |
| `TCPRAID_CLEAN` | `clean` | 数据流在两个方向都完整结束时是 `true`，被切断时是 `false` |
| `TCPRAID_REASON` | `reason` | 会话是怎么结束的，和日志里的说法一样，或者连接为什么没有被承载 |

```
{"event":"session_end","time":"2026-01-15T10:00:48.232907258Z","peer":"nas-b","rule":"zfs","session":"7dc9fd4e6c7faf75936b2d4c871cf2d4","role":"client","destination":"192.0.2.10:5000","tx_bytes":50331648,"rx_bytes":0,"clean":true,"reason":"done"}
```

不属于某个事件的变量不会被设置。原因里可能有对端发来的文字。它只出现在这些值里，绝不会出现在命令行上，而且在变量里，0 到 31 和 127 这些控制字符都换成了空格。程序拿到的是节点的环境，去掉了节点自己那些以 `TCPRAID_` 开头的变量。任何一个值里都没有密钥、令牌和配对码。

钩子在节点旁边运行，不会拖住节点。同时最多有四次运行。已经有四次在跑的时候发生的事件不交给钩子，节点会计数，并在日志里说漏掉了几个。所以只有在钩子比事件来得快的时候，它才看得到每一个事件，不能漏事件的程序还要从 API 读会话和对端。一次运行超过 `-hook-timeout` 就被结束。被结束的是节点启动的那个程序，它自己又启动的程序不在其内，所以脚本不要留下还在跑的程序。运行返回的状态不是 0 时记进日志，不影响节点。几次运行可以重叠，结束的先后也可以和事件发生的先后不同。路径测试的会话不通知钩子。

### 导出的配置

`tcpraid config export` 写出这份文件，`tcpraid config apply` 读它(`reference-cli.md`)：

```
version: 1
node:
  name: nas-a
  fingerprint: 2b7e151628aed2a6abf7158809cf4f3c762e7160f38b4da56a784d9045190cfe
  transport:
    hol: 500ms
peers:
  - name: nas-b
    fingerprint: 1979efc900f372edcde2db07d83fe6eb000973864bde0d5d2c12ab194950edf9
    allow: 192.0.2.10/32:5000
    udp_dial: ""
    transport:
      window: 8MB
    paths:
      - name: default
        type: tcp
        addr: nas-b.example:9851
        security: tls
        roles:
          - data
        subs: 8
rules:
  - name: zfs
    peer: nas-b
    listen: 127.0.0.1:5000
    destination: 192.0.2.10:5000
    enabled: true
    subs: 3
```

| 字段 | 内容 |
|---|---|
| `version` | 这种格式的版本，`1` |
| `node.name`、`node.fingerprint`、`node.transport` | 节点的名字，说明文件是从哪个节点导出的指纹，以及节点运行中存下来的传输层设置，按名字 |
| `peers[].name`、`fingerprint` | 是哪个对端。只有两者都和节点已配对的某个对端相符时，apply 才改它 |
| `peers[].allow`、`udp_dial`、`transport` | 允许列表、给这个对端的 UDP 回执发到哪里、这个对端的传输层设置 |
| `peers[].paths[]` | `name`、`type`、`addr`、`addrs`、`proxy`、`security`、`verify`、`pin`、`roles`、`subs`、`mark`，写法和 `tcpraid peers path set` 的参数相同。`proxy` 不带用户名和密码，`proxy_login` 是一条说明，表示节点里有它们 |
| `rules[]` | `name`、`peer`，`listen` 加 `destination`、`match`、`socks` 三者之一，然后是 `enabled`、`paths`、`subs`、`dial` 和 `transport`，写法和 `tcpraid rules add` 的参数相同。不写 `enabled` 的规则是开启的 |

空的列表和空的设置不写出来。apply 时遇到这个版本不认识的字段会报错。

### 网页界面的密码

它从哪里来，按下面的顺序：

1. `-ui-password-file FILE`：文件的内容，不含行尾。
2. 环境变量 `TCPRAID_UI_PASSWORD`，在用网页表单配置容器的地方很方便。值为空等于没有设置。
3. 之前用过的密码。数据目录里保存着它的加盐哈希。
4. 以上都没有，而且是第一次启动时，节点生成一个随机密码，并在日志里写一次。

```
web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
```

用文件或环境变量给出的密码会取代已保存的那个，之后即使把文件或变量拿掉，它也继续有效。想要一个新的随机密码，就停掉节点，删掉 `ui.password`，再在不带这两者的情况下启动。

带 `-no-ui` 启动的节点没有网页界面，也就没有密码。它不生成密码，不理会那个环境变量，并拒绝 `-ui-password-file`。以前某次启动留在数据目录里的 `ui.password` 会原样留着，不被使用。

## 数据目录

Linux 上默认是 `/var/lib/tcpraid`。macOS 和 Windows 上是当前用户的配置目录下名为 `tcpraid` 的目录(`~/Library/Application Support/tcpraid`、`%AppData%\tcpraid`)。系统给不出这样一个目录时，必须带 `-data`。在 Windows 上，下面列出的权限位不适用。谁能访问这些文件，由文件夹自己的权限决定，节点也不会就此发出警告。

下面省略了属主、大小和日期。

```
$ ls -la /var/lib/tcpraid
drwx------  .
-rw-------  api.addr
-rw-r--r--  api.crt
-rw-------  api.key
-rw-------  api.tls
-rw-------  api.token
-rw-r--r--  identity.crt
-rw-------  identity.key
-rw-------  lock
-rw-------  state.json
-rw-------  ui.password
```

| 文件 | 内容 |
|---|---|
| `identity.key` | 节点的 Ed25519 私钥(PKCS#8，PEM)。第一次启动时创建，之后绝不替换；密钥文件读不了是一个错误 |
| `identity.crt` | 这把密钥的自签名证书 |
| `state.json` | 节点名字、各个对端及其配对密钥和路径、未使用的配对码(编号和一个哈希)、规则。由程序写入，不要手工改 |
| `api.token` | 本机 API 的访问令牌 |
| `ui.password` | 网页界面密码的加盐哈希(PBKDF2-HMAC-SHA256)；绝不是密码本身 |
| `api.addr` | 从本机访问 API 用的地址，每次启动时写入；各条命令会读它 |
| `api.key`、`api.crt` | 带 `-api-tls` 启动过才有：一把 ECDSA P-256 私钥(PKCS#8，PEM)和它的自签名证书，API 和网页界面用它们提供。密钥只创建一次，之后绝不替换。证书有效期 800 天，启动时发现剩余不到 30 天、或者缺了某个名字，就为同一把密钥重新生成。主机名是其中一个名字，所以没有固定主机名的容器每次重建都会得到一张新证书，密钥不变 |
| `api.tls` | 只在节点上一次启动时给 API 开了 TLS 才有：API 出示的那张证书，也就是节点自己的那张，或者 `-api-cert` 给的那张。各条命令会读它，只和持有它的密钥的 API 通信。不带 TLS 启动时这个文件会被删掉 |
| `lock` | 空文件；节点运行期间锁着它 |

- **一个数据目录上同一时间只能运行一个节点。** 在同一个目录上再执行一次 `tcpraid run` 会报错退出，什么都不会改变。节点运行期间不要删除 `lock` 文件。它不是残留物，没有它，第二个节点就可能在这个目录上启动起来。
- **这个目录就是节点本身。** 只要 `identity.key` 还在，节点的指纹就不变，所以更换程序或容器时要保留这个目录。还留着 `state.json`、却丢了两个身份文件的目录会被拒绝，因为节点不会给自己造一个新身份。
- **像备份私钥那样备份它**，绝不要用同一个目录的副本运行两个节点。
- `state.json` 有格式版本。新版程序能读旧版的文件，并在下一次有改动时按自己的格式写入。它看不懂的文件会被拒绝，并保持原样。目的地是主机名的规则就是一例，0.3.0 及更早的版本不接受它。要退回这些版本之前，先把这样的规则删掉。另一例是 0.5.0 版新增的内容：有多个地址或带标记的路径，以及带 `-paths` 或 `-subs` 的规则。0.4.0 及更早的版本不接受含有它们的文件，所以退回之前先把它们去掉。`-dial` 不是 `auto` 的规则也一样，0.5.0 及更早的版本不接受它。为节点、对端或规则存下的传输层设置同样如此，0.6.0 及更早的版本不接受它。在值的位置写 `default` 就把这样的设置去掉。最新的一例是 SOCKS5 规则：0.8.0 及更早的版本不接受含有它的文件，所以退回之前先把这样的规则删掉。

## 端口一览

| 端口(默认或惯用) | 由什么设置 | 用途 |
|---|---|---|
| 9850 TCP | `-api` | 网页界面和本机 API |
| 9851 TCP | `-listen` | 其他节点，TLS |
| 任意 TCP | `-listen-plain` | 其他节点，`off` 档 |
| 任意 TCP | 规则的监听地址 | 应用 |
| 任意 TCP，习惯上用 1080 | SOCKS5 规则的地址 | 会说 SOCKS5 的应用 |
| 任意 TCP | `-transparent` | 被重定向的连接 |
| 任意 UDP | `-udp-listen` | 对端的回执 |
| 任意 TCP | `-metrics` | 节点的各项数字，不需要令牌 |

只有 9850 是程序内置的默认值。9851 和给规则用的 50000-50010 这一段，是容器镜像和 `deploy/` 里的模板所采用的。

## 限制

| | |
|---|---|
| 一个节点为自己的规则承载的会话数 | 64 |
| 一个节点作为出口的会话数 | 64 |
| 规则数 | 256，每条最多覆盖 1024 个端口 |
| 通往一个对端的路径数 | 16，每条 1 到 64 条子连接，最多 8 个地址 |
| 和一个对端之间同时进行的握手数 | 32 |
| 未使用的配对码的有效期 | 1 秒到 30 天，默认 1 小时 |
| 最坏情况下流数据占用的内存 | 两倍 `-budget`：默认值下约 512 MB |
