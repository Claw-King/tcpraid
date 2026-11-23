# 教程：两台 TrueNAS 之间的 ZFS 复制

TrueNAS 的复制任务用单独一条 TCP 连接发送数据。这篇教程让这条连接经过 tcpraid，于是它在两个站点之间走的是多条连接。

**试过什么，没试过什么。** 这篇教程里 tcpraid 这一侧——一条覆盖一段端口的规则、经每个端口发送数据、两端比较校验值——是在容器里跑的，用普通的 TCP 发送和接收程序代替两台 NAS。TrueNAS 的步骤是照着 TrueNAS 的文档和它的复制引擎的源码写的(链接在文末)。**这里没有任何一步用这个版本在真实的 TrueNAS 上试过。** 如果你系统上的界面看起来不一样，以界面为准。

## 开始之前

- 先确认它能帮上忙：在两个站点之间用 `iperf3` 测一下(一个网络测速工具：一边运行 `iperf3 -s`，另一边运行 `iperf3 -c HOST` 测一条流，运行 `iperf3 -c HOST -P 8` 测八条流)。如果八条合起来并不明显比一条快，tcpraid 不会让复制变快。
- 每个站点一个 tcpraid 节点，已经配对(见 `install-container.md` 或 `install-binary.md`)。节点可以作为应用跑在两台 TrueNAS 本机上，也可以跑在每个站点的另一台机器上。

这里用到的名字：

| | 站点 A(源) | 站点 B(目的) |
|---|---|---|
| TrueNAS | `truenas-a`，192.0.2.10 | `truenas-b`，198.51.100.20 |
| tcpraid 节点 | `node-a`，192.0.2.5(如果跑在 NAS 上就是 192.0.2.10) | `node-b` |

任务是从 `truenas-a` 到 `truenas-b` 的**推送**(push)，在 `truenas-a` 上创建。

## TrueNAS 是怎么传数据的

复制任务有一项 **Transport**。选 `SSH+NETCAT` 时，TrueNAS 只用它到另一台系统的 SSH 连接来做准备工作，数据则走另外一条不加密的 TCP 连接：

- 两台系统中的一台是 **Netcat Active Side**：用 TrueNAS 文档的话说，是 "the system that opens ports"(打开端口的那台系统)。它在 **Min Port** 和 **Max Port** 之间的第一个空闲端口上监听。
- 另一台系统连到 **Netcat Active Side Connect Address**，端口是主动一侧选中的那个。

要承载的就是这条数据连接。我们让 `truenas-b` 监听，让 `truenas-a` 去连 `node-a` 而不是直接连 `truenas-b`；`node-a` 把连接送到 `node-b`，`node-b` 再连到 `truenas-b`。

```
truenas-a --> node-a:50000-50010 ====== tcpraid ======> node-b --> truenas-b:50000-50010
 (connects)    (rule listens)                                       (netcat listens)
```

因为 `truenas-a` 连的端口号就是 `truenas-b` 选中的那个，所以规则把一段端口映射到同样的一段，逐个端口对应。

## 第 1 步：在 node-a 上为这段端口建规则

选一段在 `truenas-b` 上空闲的端口，例如 50000 到 50010。TrueNAS 每次传输取这一段里的第一个空闲端口，所以一小段就够了。

在 `node-a` 的网页界面里：规则 >“新建规则”。

- 名字：`zfs`
- 监听地址和端口：`0.0.0.0:50000-50010`
- 对端：`node-b`
- 对端那边的目的 IP 和端口：`198.51.100.20:50000-50010`

或者用命令(节点跑在容器里时，命令是 `docker exec tcpraid /tcpraid ... -data /data`，和 `install-container.md` 里一样)：

```
node-a$ tcpraid rules add zfs -peer node-b -listen 0.0.0.0:50000-50010 -dest 198.51.100.20:50000-50010
rule zfs added: listening

node-a$ tcpraid rules
NAME  TAKES                PEER    DESTINATION                SESSIONS  STATE
zfs   0.0.0.0:50000-50010  node-b  198.51.100.20:50000-50010  0         listening
```

节点作为容器跑在 NAS 上时，有两点要留意：

- 在 `truenas-a` 上，`node-a` 的容器必须把这一段端口映射出来(`deploy/` 里的 compose 文件映射的是 50000-50010)。
- 在 `truenas-b` 上，这一段端口必须在 **NAS 本机上是空闲的**，因为 TrueNAS 要在其中一个上监听。如果 `node-b` 的容器映射了同一段端口，就在那边去掉这个映射(`node-b` 上没有需要它的规则)，或者给复制换一段端口。

目的地写 `truenas-b` 的局域网地址，即使 `node-b` 就跑在 `truenas-b` 上也一样：在容器里，`127.0.0.1` 是容器自己。

## 第 2 步：SSH 连接

TrueNAS 仍然需要它从 `truenas-a` 到 `truenas-b` 的 SSH 连接(Credentials > Backup Credentials > SSH Connections)。它上面的流量很少。分两种情况：

- `truenas-a` 已经能连到 `truenas-b` 的 SSH 端口(有 VPN，或者有端口转发)：SSH 连接保持原样。
- 连不到：在 `node-a` 上再加一条规则，例如 `-listen 0.0.0.0:50022 -dest 198.51.100.20:22`，并在 SSH 连接里(设置方式选 Manual)把 **Host** 填成 `node-a` 的地址，**Port** 填 50022。tcpraid 原样传递字节，所以 `truenas-a` 看到的主机密钥仍然是 `truenas-b` 的。和任何手动设置的 SSH 连接一样，TrueNAS 还需要一把私钥，它对应的公钥要被 `truenas-b` 接受。如果 `node-a` 跑在容器里，50022 端口也要映射出来：在它的 compose 文件的 `ports:` 里加上 `- "50022:50022"`(它默认映射的那一段已经被 `zfs` 规则占用了)，或者让容器共用主机的网络。

## 第 3 步：复制任务

在 `truenas-a` 上：Data Protection > Replication Tasks > Add 打开向导；其中的 **Advanced Replication Creation** 打开 Add Replication Task 界面，TrueNAS 文档就是在这个界面上描述 `SSH+NETCAT` 各项设置的。源、目的、快照和计划照你不用 tcpraid 时那样设。这里要紧的几项：

| 设置项 | 值 |
|---|---|
| Direction | Push |
| Transport | SSH+NETCAT |
| SSH Connection | 到 `truenas-b` 的那个连接 |
| Netcat Active Side | Remote(这样由 `truenas-b` 监听) |
| Netcat Active Side Listen Address | 留空 |
| Netcat Active Side Min Port | 50000 |
| Netcat Active Side Max Port | 50010 |
| Netcat Active Side Connect Address | `node-a` 的地址：`192.0.2.5`(如果 `node-a` 跑在 `truenas-a` 上就是 `192.0.2.10`) |

不填连接地址的话，TrueNAS 会去连 SSH 连接里的那台主机，也就是直接连 `truenas-b`，tcpraid 就不在这条路上了。

监听地址留空：这样 TrueNAS 会在 `truenas-b` 的所有地址上监听。根据编写本教程时查阅的复制引擎源码，在这一项里填 IPv4 地址(包括 `0.0.0.0`)会让负责监听的辅助程序在启动时失败，尽管文档页面上说 `0.0.0.0` 是默认值。

## 第 4 步：运行并查看

运行任务。运行期间，`node-a` 的网页界面在“会话”页里显示一个会话，带着它的规则(`zfs`)、速度和子连接数，`tcpraid rules` 在 SESSIONS 一栏显示 `1`。在 `node-b` 上，同一个会话显示为由对端发起。

如果任务一开始就失败，看 `node-a` 上规则的状态(“规则”页，或者 `tcpraid rules`)：它会说明连接为什么没能被承载。常见原因列在 `troubleshooting.md` 里。

如果复制过程中两个站点之间的线路断了，两个节点会把连接保持最长 15 分钟，线路恢复后接着传。如果有节点重启，连接会被重置，TrueNAS 看到的情形和任何一条断掉的连接一样。

## 哪些部分没有受到保护

TrueNAS 的文档说明 `SSH+NETCAT` 的数据流是不加密的，适用于可信的网络。按这里的做法：

- 在 `node-a` 和 `node-b` 之间，数据流在 TLS 1.3 里面(除非你把路径的档位设成 `off`)；
- 在 `truenas-a` 和 `node-a` 之间，以及 `node-b` 和 `truenas-b` 之间，它在各自站点的局域网上是不加密的。节点就跑在 NAS 本机上时，这一段不出机器。

## 另一个方向：拉取任务

拉取(pull)任务在目的端 `truenas-b` 上创建，从 `truenas-a` 取数据。数据仍然从 A 流向 B，应该发起连接的仍然是 `truenas-a`。根据同一份文档推断(没有跑过)：把 **Netcat Active Side** 设成 Local，这样由 `truenas-b` 监听；把 **Netcat Active Side Connect Address** 设成 `truenas-a` 连 `node-a` 时用的那个地址；`node-a` 上的规则和第 1 步里一样。

## 出处

- TrueNAS 文档，Replication Task 界面：Transport 的各个选项，以及上面引用的 `SSH+NETCAT` 设置。
  `https://www.truenas.com/docs/scale/dataprotection/replication/replicationscreens/`
- TrueNAS 文档，SSH Connections 界面(手动连接的 Host 和 Port)。
  `https://www.truenas.com/docs/scale/credentials/backupcredentials/sshcredentialscreens`
- zettarepl，TrueNAS 的复制引擎：`ssh_netcat.py`(被动一侧连到连接地址，端口是主动一侧选中的那个；没有连接地址而主动一侧是 Remote 时，用 SSH 连接里的主机；没有监听地址时在 `::` 上监听，即所有地址)和 `ssh_netcat_helper.py`(一个监听，接受一条连接；它要的是同时接受 IPv4 和 IPv6 的套接字，而 IPv4 的监听地址给不了)。`https://github.com/truenas/zettarepl`
