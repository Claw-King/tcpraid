# 排错

先看这三处：**规则的状态**(“规则”页，或 `tcpraid rules`)、**对端的状态**(“对端”页，或 `tcpraid peers`)，以及**节点的日志**(`docker logs tcpraid`、`journalctl -u tcpraid-node`)。

## 应用连不上，或者一连上就被断开

规则承载不了某条连接时，会重置它，并记下原因和时间。所以连接失败时，先在规则所在的节点上看 `tcpraid rules`：

```
nas-a$ tcpraid rules
NAME  TAKES         PEER   DESTINATION         SESSIONS  STATE
back  0.0.0.0:9811  nas-b  198.51.100.20:7100  0         listening, last connection: this node has no path to peer "nas-b", and the peer, which would open the connections, is not connected to this node. Start the peer, or add a path with "tcpraid peers path set".

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
taken  0.0.0.0:9700  nas-a  192.0.2.10:873  0         FAILED: listening on 0.0.0.0:9700 failed: listen tcp 0.0.0.0:9700: bind: address already in use
```

| 你看到的 | 含义 |
|---|---|
| `STATE` 是 `FAILED`，后面带原因 | 这条规则没能监听：端口被占用，或者不被允许。其他规则照常运行。任何规则有改动时，节点会再试一次 |
| "this node has no path to peer ... and the peer is not connected" | 对端离线，而本节点没法拨过去。启动对端，检查它通往本节点的路径 |
| 在这一切之前就被拒绝连接(connection refused) | 那个端口上没有东西在监听。在容器里：规则的端口映射出来了吗？规则是不是监听在 `0.0.0.0` 上？ |
| "peer ... could not connect to ADDRESS: connect: connection refused"(或者系统给出的别的错误) | 远端节点去连目的地，没连上。那里没有东西在监听，或者从远端节点看过去这个地址不对 |
| "peer ... refused the destination ADDRESS, which is not in the allow list it keeps for this node" | 远端节点给本节点设的允许列表里没有这个目的地。在远端节点上用 `tcpraid peers set` 改 |
| "peer ... could not find the address of NAME: no such host" | 规则的目的地是主机名，而负责解析它的远端节点不认识这个名字。在远端节点上检查这个名字，或者给规则写 IP 地址 |
| "peer ... could not find the address of NAME: the lookup failed. The log of that node says why." | 远端节点的解析器给出的是“no such host”以外的错误，比如它的域名服务器没有应答。远端节点的日志里有这个错误 |
| "peer ... does not look up host names for this node (it runs version ...)" | 远端节点的版本早于 0.4.0。升级它，或者给规则写 IP 地址 |
| "peer ... refused the destination NAME:PORT, because the name stands for no address in the allow list it keeps for this node" | 远端节点给本节点设的允许列表里，没有这个名字在这个端口上的任何地址。在远端节点上用 `tcpraid peers set` 修改 |
| "peer ... did not say within 10s which address NAME stands for" | 远端节点没有及时回答这个名字对应哪个地址。重试这条连接。如果一直这样，看远端节点的日志 |
| "peer ... looks up as many host names as it does at a time, and did not look up NAME" | 远端节点正忙着解析别的名字。稍后再试 |
| "peer ... is not connected to this node, so it cannot be asked which address NAME stands for" | 主机名要经两个节点之间的连接去问，而这条连接断着。`tcpraid peers` 会显示对端离线。见“对端离线” |
| "peer ... could not connect to ADDRESS. Its log says why." | 还是上面两种情况，但远端节点的版本早于 0.3.0，不会把原因报回来。它的日志里有 `dial ADDRESS: ... connection refused`，或者 `dial ADDRESS: not in the allow list of the peer` |
| 15 秒后被重置，规则在没有路径的节点上 | 已经请对端连过来，但什么都没到：对端已经连不到本节点的监听了 |
| "peer ... does not answer on the connections this node opens"，30 秒后变成 "peer ... answered on none of the connections within 30 seconds" | 子连接一条都没建起来，后面的文字说明那些尝试碰到了什么。原因之一：另一个节点上已经解除了配对。这时规则会说 "the other node does not accept this node, which is not paired with it or no longer is"，另一个节点的日志里有 "refused a connection from ...: not a peer of this node" 这样的行。重新配对 |
| "the connections to peer ... cannot be opened"，后面带一个错误 | 本节点根本连不上对端的监听。用 `tcpraid peers check NAME` 逐条试路径 |
| "nothing arrived from peer ... for the whole grace time" | 两个节点之间的线路断开的时间超过了 `-grace`。见下一节 |
| "peer ... was restarted while it carried a connection of this rule, and the connection was reset" | 另一个节点作为新的进程回来了(它被重启、被杀掉，或者内存耗尽)，本节点把旧进程承载着的那条连接重置了。重新开始传输即可。如果反复出现，去看另一个节点的日志和它的内存上限(`-maxrss`、它所在容器的上限) |

目的地是**从远端节点看过去的**地址。远端节点跑在容器里时，`127.0.0.1` 是容器，不是 NAS。

## 传输不动了

`tcpraid sessions` 会告诉你会话是不是在等线路：

```
ID      ROLE    RULE   PEER   DESTINATION     AGE    SENT     RECEIVED  AVERAGE   NOW       SENT AGAIN  SUBS  NOT SENT YET  IN FLIGHT  STATE
4a5411  client  rsync  nas-a  192.0.2.10:873  5m12s  2.10 GB  0 B       6.9 MB/s  0.0 MB/s  1.0%        0     104.0 MB      24.0 MB    waiting for the peer for 3m12s, gives up in 11m48s
```

| `STATE` | 含义 |
|---|---|
| `ok`，而 `NOW` 是零 | 两个节点之间还在通话，只是没有数据在走。应用没有在发，或者目的地没有在读 |
| `waiting for the peer for ..., gives up in ...` | 已经这么久没有从对端收到任何东西。线路断了，或者对端停了。会话会保持着，线路恢复后接着传。保持时间用完时，应用的连接被重置 |
| `connecting` | 对端还没有在任何子连接上应答。`tcpraid rules` 会说明那些尝试碰到了什么 |

`tcpraid sessions -watch` 每 2 秒打印一次表格。要扛过更长的断线，需要把两个节点的 `-grace` 都设长一些(见 `reference-files.md`)。

## 对端离线

`online` 表示两个节点之间的控制连接连着。持有路径的节点负责打开它，断了每 8 秒重试一次。

```
nas-b$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok
```

`peers check` 在每条路径上打开一条连接，并让对端证明它持有这个配对。如果失败，报错会说明问题出在哪里：

- `FAILED: dial tcp ...: connect: connection refused`(或者超时)：查地址、途中的端口转发，或者对端的 `-listen` 端口。
- `FAILED: tls: first record does not look like a TLS handshake`：这个地址通向的不是节点的 TLS 监听，例如连到了节点的 9850 端口而不是 9851。
- `FAILED: ... connection reset by peer. The other node closed the connection during the handshake and may be busy. Try again.`，而且每次都这样：除了节点正忙之外，`tls` 档的路径连到节点的明文监听(`-listen-plain`)时得到的也是这个，报错的后半句会提到。检查端口和路径的档位是否对应。
- "the security tiers of the two ends do not match. This path is of the owncert tier, and the listener at ... has no certificate of its operator to show."：这条路径要对方出示自己的证书，而对方启动时没有带 `-cert` 和 `-key`。让对方带上它们启动，或者把路径的档位改成 `tls`。
- "the node at this address is not the peer. Its key has another fingerprint."：在那个地址上应答的是另一个节点，或者对端因为数据目录丢了而换了新的身份。
- "the other node does not accept this node, which is not paired with it or no longer is"：另一个节点上已经解除了配对。
- "the other end closed the connection without answering"：在 `off` 档的路径上，是另一个节点没有和本节点配对，或者这个地址是对方的 `-listen`，那里只接受 TLS。在这样的路径上，报错会把第二种原因写出来。在任何一档，可能是一次被打断的配对之后两边持有不同的配对密钥，或者两边的时钟相差超过十分钟，这时另一个节点的日志会写明。重新配对，或者校准时钟。

没有通往对端路径的节点没法检查，它会说 `this node has no path to ... and can only be reached by it`。请从另一边检查。

## 配对失败

- "no address for the other node to connect to"：节点不知道自己对外的地址。生成配对码时用 `-addr HOST:PORT` 或者网页界面里的对应字段给一个，或者启动节点时加上 `-advertise`。
- "the other node did not accept the pairing code"：配对码已经用过、已经过期(默认一小时)、已被作废，或者没有复制完整。
- "the pairing code is damaged"：重新复制，要完整、不能有改动。
- "the node at this address is not the one that made the pairing code"：这个地址通向的是另一个节点。
- 完成不了的加入会在 60 秒后放弃。

## 能用，但没有变快

- 用 `tcpraid peers test NAME` 测一下子连接带来了多少(见 `reference-cli.md`)。它在每条路径上发测试数据，一次只用一条子连接，一次用全部。如果全部合起来并不明显比一条快，tcpraid 在这条路径上就没有什么可以提升的。要么一条连接已经占满了线路，要么路径上有东西把这几条连接合进了一条，`connecting.md` 讲了怎么分辨。用 `iperf3` 分别跑一条流和八条流，可以在不用 tcpraid 的情况下测同一件事。
- 在“会话”页、用 `tcpraid sessions` 或 `GET /api/v1/sessions` 看会话。`subs` 应该等于各条路径的连接数之和(默认 8)，`resent_bytes` 应该只占 `tx_bytes` 的一小部分，命令里的 `SENT AGAIN` 一列显示的就是这个比例。重发的字节很多，说明路径在丢包或者卡顿。这时有帮助的是第二条承担 `repair` 和 `control`(网页上勾“补发”和“回执”)的路径，或者 UDP 回执(见 `concepts.md`)。
- 应用和它的节点之间、远端节点和目的地之间这两段的速度同样算数，因为 tcpraid 快不过其中较慢的一段，也快不过应用本身。
- 会话结束时，两个节点各自会为它记两行日志：字节数、重发的块数、被换掉的子连接数、卡住的时间。用 `-timeline` 启动的节点在会话进行中也会记同一类数字。下一节解释这些数字，以及怎样从中看出要不要改 `-window` 或 `-hol`(`reference-files.md`)。

## 读懂日志里的会话行

会话结束时，每个节点会为它记两行日志。用 `-timeline 2s` 启动的节点，在会话进行中还会每两秒为每个会话记一行，第一行在会话开始后大约半秒。这些数字是记日志的节点自己的。发送方面的数字在发送端有意义，接收方面的数字在另一端有意义。

### 时间线那一行

```
[ddc465] tl t=3.0 tx=34.2 re=0.0 ack=37.1 rx=0.0 dlv=0.0 unacked=0 unsent=0 pend=0 infl=0 sacked=63 fbage=0.03 fb=144/9 fbc=0 fbu=0 cap=0/8 holage=0.00 pumpwait=0.28 starve=0.94 loop=243/1521
```

速率是从上一行到这一行之间的 MB/s，大小是记这一行那一刻的 MB。

| 字段 | 含义 |
|---|---|
| `t` | 会话开始以来的秒数 |
| `tx` | 第一次发出的流数据 |
| `re` | 重发的流数据 |
| `ack` | 对端报告已收到的数据；报告到了，这边就把它释放 |
| `rx` | 从对端收到的数据，包括重复收到的 |
| `dlv` | 按顺序写给本侧应用的数据 |
| `unacked` | 已从应用读进来、对端还没有报告收到的数据。其中包括 `unsent` |
| `unsent` | `unacked` 里还没有发出去的部分：它在等某条子连接有空位 |
| `pend` | 已收到、还没有写给应用的数据：它在等前面的某个块，或者在等应用来取 |
| `infl` | 在途的数据：已在承载数据的子连接上发出、对端还没有报告收到 |
| `sacked` | 对端的 `pend`：对端已收到、还没有交付的数据 |
| `fbage` | 距离对端上一份报告到达过了多少秒(`-1`：还没有收到过) |
| `fb=IN/OUT` | 上一行以来收到和发出的报告数(发出的里面不计经 UDP 发的那一份) |
| `fbc`、`fbu` | 收到的报告里，经控制连接来的和经 UDP 来的各有多少 |
| `cap=A/N` | 承载数据的 `N` 条存活子连接(角色 `data` 和 `repair`)里，有 `A` 条的在途数据已经到了允许的上限 |
| `holage` | 接收方面：`pend` 里最老的数据为了等它前面缺失的块，已经等了多少秒 |
| `pumpwait` | 上一行以来，因为 `-window`、对端的窗口或 `-budget` 已满，本节点没有从应用读数据的时间占比。一次等待记在它结束时的那一行里：跨了好几行的一次长等待，在那几行里显示为 0，然后在结束的那一行里显示为大于 1 的值 |
| `starve` | 平均每条数据子连接“有空位却没有数据可发”的时间占比。`repair` 路径的子连接也算在内，它们大部分时间是空闲的 |
| `loop=P50/P95` | 从在一条子连接上发出数据，到“它已到达”的报告回来，经过的毫秒数：中位数和 95 分位。数据在途中排队的时间也包含在内。一条没有新数据可发的子连接会一直报告它最后那批数据，所以子连接空闲时这个数字会越来越大(`repair` 路径的子连接也在其中) |

是什么在限制一次传输，在发送端的节点上看得出来：

- `pumpwait` 接近 0，同时数据在走(`tx` 大于零)：节点在等应用，限制不在路径上。连续几行 `pumpwait` 为 0 而 `tx` 也为 0，是一次还没结束的长等待，要按下面两种情况来看。
- `pumpwait` 接近 1，`unsent` 很大，数据路径的每条子连接都到了上限：应用给数据的速度比路径带走的快。窗口里装满了还没发出去的数据，所以把 `-window` 加大只会多装一些，不会发得更快。
- `pumpwait` 接近 1，`unsent` 接近 0：数据已经发出去了，窗口里装满的是对端还没报告、或者还交付不了的数据，`sacked` 很大。这时去看接收端的节点。如果它的 `holage` 大于零，说明那里的数据在等缺失的块。两个节点都把 `-window` 加大，可以给它更多空间。在发送端的节点上把 `-hol` 调短，缺失的块会更早重发，代价是有些块会发两遍。如果它的 `holage` 一直是零而 `pend` 很大，说明那边的应用取数据慢，tcpraid 的任何设置都改变不了这一点。它结束时那一行里的 `stall` 给出了总共慢了多久。如果它的 `holage` 是零，`pend` 也很小，说明那里没有数据在等。这时窗口比路径上能容纳的在途数据还小，两个节点都把 `-window` 加大，可能会多走一些。

### 结束时的两行

```
[ddc465] session end: done (6.2s tx 200MB rx 0MB chunks 800 resends 0 kicks 0 subs 8 stall 0.0s)
[ddc465] session diag: pump win 0.7s | resent | subs died lost=0 silent=0 write=0 slow=0 | fb gap max 0.0s >1s 0 | gap repairs 0 (retries 0) ms p50/p95/max: peer wait 0/0/0, queued 0/0/0, to release 0/0/0 | snapshots truncated 0
```

`session end`(或 `session aborted`)先给出原因，然后是：

| 字段 | 含义 |
|---|---|
| 第一个数字 | 会话持续了多久 |
| `tx`、`rx` | 发出(只算第一次)和收到的流数据 |
| `chunks` | 第一次发出的块数 |
| `resends` | 被排进重发队列的块数 |
| `kicks` | 因为比同一路径上的其他子连接慢得多而被换掉的子连接数 |
| `subs` | 会话存续期间建立起来的子连接数 |
| `stall` | 花在“单次超过 50 毫秒的、写给应用的写操作”上的时间：应用，或者到应用的那一段，取数据慢 |

`session diag`：

| 字段 | 含义 |
|---|---|
| `pump win` | 因为窗口或预算已满，本节点没有从应用读数据的总时间(就是 `pumpwait` 背后的那个总和) |
| `resent gap=N stall=N dead=N` | 块为什么被排进重发队列：对端报告后面的数据在等它(`gap`)；过了应到的时间很久，对端仍没有报告收到它(`stall`)；承载它的子连接断了(`dead`)。次数为零的原因不显示 |
| `subs died lost / silent / write / slow` | 子连接是怎么结束的：连接出错或被关闭(`lost`，其中包括会话结束时对方节点关掉的那些)；20 秒没有进展(`silent`)；一次写操作没有按时完成(`write`)；因为慢而被换掉(`slow`) |
| `fb gap max`、`>1s N` | 有未确认数据时，对端相邻两份报告之间隔得最久的一次；以及超过一秒的次数 |
| `gap repairs N (retries R)` | 因为对端报告缺失而重发、之后对端又报告已收到的块数(数到 4096 为止；全部的次数在 `resent gap=` 里)；以及为缺失的块重发时，不是该块第一次重发的次数 |
| `peer wait` | 针对这些块：本节点决定重发它的那一刻，对端后面的数据已经等了多久。**它不是路径的性质。** 只有等待超过 `-hol` 加上允许的排队量(300 毫秒；报告中断之后的一段时间里是 1 秒)才会做这个决定，所以在默认设置下它不会低于 1.3 秒，通常只比 1.3 秒多一点 |
| `queued` | 从做出决定，到这个块被交给某条子连接 |
| `to release` | 从做出决定，到对端报告收到了这个块 |
| `snapshots truncated` | 本节点生成的报告里，有多少份列不下它持有的全部乱序区间 |
| `udp in / out / gap max` | 使用 UDP 回执时：这个会话接受和发出的数据报数，以及相邻两个被接受的数据报之间隔得最久的一次 |

三个时间的单位是毫秒，依次是中位数、95 分位、最大值。

### `-stats` 的那几行

用 `-stats 5m` 启动的节点每五分钟记一次它的合计数字，后面跟着当时它承载的每个会话各一行：

```
stats sessions=1 goroutines=28 rss=20MB heap=6MB unacked=0MB resends=0 kicks=0/0/0 rxdrop=0 udp in/out/bad/toobig/old=0/0/0/0/0
[8ac974] 127.0.0.1:7000 9s tx 242MB rx 0MB unacked 0MB sacked 128MB pending 0MB subs: [p0 2277/1812KB/s loop0ms infl0KB cap684KB] [p0 2624/776KB/s loop0ms infl0KB cap790KB]
```

`stats` 那一行。计数器从节点启动时算起，其余的是那一刻的状态。

| 字段 | 含义 |
|---|---|
| `sessions` | 节点现在承载的会话数 |
| `goroutines` | 进程里的 goroutine 数(随会话和子连接增减；只增不减的话值得报告) |
| `rss` | 进程的常驻内存，`-maxrss` 比较的就是它。在 macOS 和 Windows 上显示为 `n/a`，程序在那里不读这个值 |
| `heap` | Go 运行时的堆正在使用的内存 |
| `unacked` | 所有会话合计：从应用读进来、对端还没报告收到的数据；`-budget` 限制的就是它 |
| `resends` | 计数器：排队等待重发的块数 |
| `kicks=A/B/C` | 计数器：本节点换掉的子连接数——因为比同一路径上的其他子连接慢得多(`A`)，20 秒没有进展(`B`)，一次写入没有按时完成(`C`) |
| `rxdrop` | 计数器：到达时因为乱序数据的空间已满而被丢掉的块数；对端会重发 |
| `udp in/out/bad/toobig/old` | UDP 回执的计数器：接受的数据报；发出的数据报；被拒绝的(不属于本节点的会话，或者认证失败)；因为太大装不进一个数据报、只走了 TCP 连接的报告；通过了认证、但不比已经见过的更新的数据报 |

会话的那一行：

| 字段 | 含义 |
|---|---|
| `[8ac974]` | 会话号的前六位 |
| `127.0.0.1:7000` | 这条连接的目的地 |
| `9s` | 会话已经运行了多久 |
| `tx`、`rx` | 发出的流数据(只算第一次发送)和收到的流数据，和结束时那一行相同 |
| `unacked`、`sacked`、`pending` | 就是时间线里的 `unacked`、`sacked` 和 `pend` |
| `subs:` | 每条活着的子连接一组 |
| `p0` | 它属于哪条路径：在拨出节点为对方保存的路径列表里，这条路径的序号，从 0 开始 |
| `2277/1812KB/s` | 对端报告的、经这条子连接交付的速率：一秒内的最高值，和最近两秒的平均值 |
| `loop0ms` | 在这条子连接上，从发出数据到“它已到达”的报告回来，见过的最短时间 |
| `infl` | 在这条子连接上已经发出、还没被报告收到的数据 |
| `cap` | 这条子连接最多可以有多少在途数据 |
| `[ctrl p1]` | `control` 路径的子连接：只传报告，不传数据 |

在接收数据的节点上，速率和 `infl` 都是零，因为它不发送流数据。

## 网页界面

- 忘了密码，或者日志里显示生成密码的那一行已经找不到了，比如容器启动了很久：自己设一个。在容器里，把它写进环境变量 `TCPRAID_UI_PASSWORD`，compose 文件和各个模板里都有这一项，再用 `docker compose up -d` 重新创建容器。单纯重启容器用的还是旧的环境变量。用二进制文件时，用这个变量或 `-ui-password-file FILE`。它会取代旧密码(见 `reference-files.md`)。如果想要一个新的生成密码，就停掉节点，删掉数据目录里的 `ui.password`，再启动节点。新密码会在日志里显示一次。
- "too many wrong passwords from this address"：等一分钟。
- 同一台主机上有两个节点时反复被登出：两者用的是同一个 cookie 名字。请给每个节点用各自的主机名来访问。
- 从别的机器打不开页面：API 监听在 `127.0.0.1`，除非用 `-api` 另外指定。

## 节点启动不了

- "the data directory ... is in use by another tcpraid"：一个目录只能有一个节点，而前一个还在运行。
- "... has a state file but neither identity.key nor identity.crt. Its identity is lost, and a new one is not created in its place."：从备份里恢复这些文件，或者用一个空目录启动并重新配对。
- `tcpraid status` 说 "the node is not running or -data DIR is not its data directory"：这些命令是通过数据目录找到节点的。给出和节点相同的 `-data`，在容器里是 `-data /data`。
- 数据目录或其中的密钥文件能被其他人读或写时，日志会给出警告。请修正权限。

## 用于监控的数字

`GET /metrics` 以 Prometheus 文本格式给出计数器，需要令牌：

```
# TYPE tcpraid_sessions_total counter
tcpraid_sessions_total 6
# TYPE tcpraid_sessions_aborted counter
tcpraid_sessions_aborted 0
# TYPE tcpraid_sessions_active gauge
tcpraid_sessions_active 1
```

用 `-stats DURATION` 启动的节点会按这个间隔把它的合计数字写进日志，供读日志的脚本使用(见上面的“`-stats` 的那几行”)。

没有节点应答时，`tcpraid status` 以错误状态退出，容器的健康检查用的就是它。
