# 教程：用另一个站点的文件共享

站点 A 有一台文件服务器，站点 B 有一台电脑，像在同一栋楼里那样挂载它的共享。这篇文档讲的是这样做的速度，它更多取决于电脑怎样要数据，而不是 tcpraid。

**试过什么。** 一位用户在一个站点的电脑上，经路由器上的透明规则，跨一条长距离的线路挂载了另一个站点的 NAS 上的文件共享，并用一个基于 mpv 的播放器从上面播放视频。**没有量过的：**同一个共享在同一条线路上不经 tcpraid 时怎么样。

## 环境

| | 站点 A | 站点 B |
|---|---|---|
| 有 | 文件服务器，在 192.0.2.10 | 挂载共享的电脑 |
| tcpraid 节点 | `nas-a` | `gw-b`，在站点 B 的路由器上 |

两个节点已经配对(见 `install-container.md` 或 `install-binary.md`)。这个例子里文件服务器在 445 端口监听。

## 第 1 步：站点 B 的节点上建一条规则

站点 B 的路由器上有一个以 `-transparent` 启动的节点(再加 `-redirect`，或者自己写重定向规则，见 `reference-files.md`)时，一条透明规则把去文件服务器的连接接过来，电脑用和在站点 A 时相同的地址挂载共享：

```
gw-b$ tcpraid rules add share -peer nas-a -match 192.0.2.10/32:445 -coalesce 0s
rule share added: listening, with coalesce=0s
```

路由器上没有节点时，站点 B 另一台机器上的节点也行。它同样和 `nas-a` 配对，用一条固定规则做同样的事，电脑改为在这台机器的地址上挂载共享。它的 445 端口要空着，所以它不能是自己也提供文件共享的机器：

```
node-b$ tcpraid rules add share -peer nas-a -listen 0.0.0.0:445 -dest 192.0.2.10:445 -coalesce 0s
```

`-coalesce 0s` 让节点把应用给它的每一块数据立刻发出去，不再等着凑满(见 `reference-files.md`)。发一个小请求、等着回答的应用需要这样。文件的数据是从站点 A 发出来的，所以这项设置在 `nas-a` 上也要起作用。两个节点都是 0.12.0 或更新的版本时，规则上的设置会做到这一点。站点 A 是更旧的节点时，改在它上面为对端设：在 `nas-a` 上运行 `tcpraid peers set gw-b -coalesce 0s`。

## 第 2 步：速度由什么决定

经文件共享读一个文件，是一连串的请求和回答。以 SMB 2 为例，客户端发一个 READ 请求，写明从文件的哪里读、读多少字节，服务器在 READ 回答里把数据送回来(`[MS-SMB2]` 的 2.2.19 和 2.2.20 节)。每个回答至少要在线路上走一个往返。所以速度大约是电脑一次要多少、同时有几个请求在路上，再除以往返的时间。距离远时，这可能远低于线路能承载的。除了第 1 步里的 `-coalesce 0s`，tcpraid 的哪项设置都改变不了它。上面那位用户是在一台电脑、一个播放器上看到这种情形的。

这时在发送数据的站点 A 的节点上，`tcpraid sessions` 的状态是 "ok, waiting for the application"，它的“会话”页上写着“在等应用给数据”(见 `troubleshooting.md` 的“能用，但没有变快”)。

想对比一下，可以从共享上复制一个大文件到本地硬盘，计一下时间。如果复制明显比你关心的程序快，限制就在那个程序上，要改的地方也在那个程序里。

## 第 3 步：播放视频

播放器如果每次只读一点，要用到下一段时才去要，在远距离上就会卡，尽管线路还有余量。办法是让播放器一次读得多一些、提前读得远一些。

以 mpv 播放器为例，它有这几个选项(引自它的手册)：

| 选项 | 手册里怎么说 |
|---|---|
| `stream-buffer-size` | "Size of the low level stream byte buffer (default: 128KB)."它用作解复用和底层读取(比如从套接字读)之间的缓冲 |
| `demuxer-max-bytes` | "This controls how much the demuxer is allowed to buffer ahead." |
| `demuxer-readahead-secs` | "If --demuxer-thread is enabled, this controls how much the demuxer should buffer ahead in seconds (default: 1)."手册接着说："--cache-secs will override this. Technically, the maximum of both options is used." |

在 mpv 的配置文件里，选项去掉前面的两个横线，写成 `option=value`。大小可以带 `KiB`、`MiB` 这样的后缀。下面的数值是给经长距离线路读数据的播放器的一个例子，不是在哪里都最好的数值：

```
stream-buffer-size=16MiB
demuxer-max-bytes=1024MiB
demuxer-readahead-secs=120
```

## 说明

- 文件服务器看到的每个客户端都来自站点 A 的节点。文件服务器上按客户端地址做的限制，分不出站点 B 的各个客户端了。
- 两个节点之间的连接默认是加密的(见 `security.md`)。
