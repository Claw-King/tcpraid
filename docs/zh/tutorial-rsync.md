# 教程：用 rsync 连到 rsync 守护进程

rsync 有两种方式和远端系统通信：经过 ssh 这样的远程 shell，或者经 TCP 直接连到 **rsync 守护进程**，通常是 873 端口。rsync 运行一次，和守护进程之间用的是一条 TCP 连接；它的手册里没有用多条连接的选项。这篇教程让这条连接经过 tcpraid。

**试过什么。** 就是下面这些命令，在容器里跑的：一个节点后面是真的 rsync 守护进程，另一个节点上建规则、用真的 rsync 客户端经规则连过去，三个文件共 43 MB；之后用 `diff -r` 比较，没有差异。**没试过的：**任何 NAS。文末关于 NAS 产品的说明来自它们的文档。

## 环境

| | 站点 A | 站点 B |
|---|---|---|
| 有 | rsync 守护进程，在 192.0.2.10 的 873 端口 | 要发送的数据 |
| tcpraid 节点 | `nas-a` | `nas-b` |

两个节点已经配对(见 `install-container.md` 或 `install-binary.md`)。

## 第 1 步：站点 A 上的守护进程

如果你已经有 rsync 守护进程(NAS 上的“rsync 服务器”)，跳过这一步。下面是一个测试用的最小配置，带一个叫 `backup` 的模块：

```
# /etc/rsyncd.conf
use chroot = no
[backup]
    path = /srv/backup
    read only = no
    uid = root
    gid = root
```

```
site-a# rsync --daemon --config=/etc/rsyncd.conf --port=873
```

这个守护进程接受任何能连到它的人。实际使用时，请按 rsync 文档(`rsyncd.conf`)所说的限制谁能使用这个模块。有一点要注意：经过规则之后，守护进程看到的每个客户端都来自 `nas-a`，所以按客户端地址做限制(`hosts allow`)已经分不出谁是谁——凡是能连到 `nas-b` 上规则端口的人，到了那边都成了 `nas-a`。请改用用户名和密码来限制(`auth users` 和 `secrets file`)，并且让规则只在你想放行的客户端连得到的地址上监听。

## 第 2 步：在 nas-b 上建规则

在 `nas-b` 的网页界面里：规则 >“新建规则”，监听地址和端口填 `0.0.0.0:8873`，对端选 `nas-a`，目的地填 `192.0.2.10:873`。或者：

```
nas-b$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:8873 -dest 192.0.2.10:873
rule rsync added: listening
```

`nas-b` 跑在容器里时，用容器映射出来的端口，例如把 8873 换成 50000。

## 第 3 步：让 rsync 连规则

把守护进程的地址和端口换成 `nas-b` 的地址和规则的端口，交给 rsync：

```
site-b$ rsync -a --stats /data/ rsync://nas-b.example.org:8873/backup/

Number of files: 5 (reg: 3, dir: 2)
Number of created files: 4 (reg: 3, dir: 1)
Number of regular files transferred: 3
Total file size: 43,000,006 bytes
Total transferred file size: 43,000,006 bytes
...
sent 43,010,778 bytes  received 85 bytes  86,021,726.00 bytes/sec
total size is 43,000,006  speedup is 1.00
```

运行期间，规则上会显示一个会话：

```
nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:8873  nas-a  192.0.2.10:873  1         listening
```

反方向，也就是从守护进程拉取，是同一条命令，把源和目的对调。无论数据往哪个方向流，规则都承载这条连接。

## 保护了什么

rsync 守护进程协议本身不加密。在两个节点之间，这条连接在 TLS 1.3 里面；在每个站点上 rsync 和本地节点之间是明文，走的是局域网。

## NAS 产品上的 rsync

在 TrueNAS 以外的 NAS 产品上，已知适合 tcpraid 的传输就是 rsync 连守护进程；它们其他的复制功能开一条连接还是多条，厂商文档没有说明，也没有测试过。

- **群晖**：文档里列出了 873 端口，用于不加密传输的 rsync(共享文件夹同步、Hyper Backup 备份到 rsync 服务器)。
- **QNAP**：Hybrid Backup Sync 有到远端服务器的 rsync 任务，带一个填端口的字段。
- **TrueNAS**：Rsync Tasks 有 "Module" 模式，它连的是守护进程。它的文档页面上没有显示填守护进程端口的字段。

产品允许设置服务器地址和端口的，填节点的地址和规则的端口。只允许设置地址的，让规则监听 873 端口——这只有在节点所在的主机上没有别的东西占用这个端口时才行，所以在自己运行着 rsync 服务器的 NAS 上不行。这些都没有在产品上试过。

## 出处

- rsync 手册页：和远端系统通信的两种方式；"Connections to an rsync daemon typically use TCP port 873"。
  `https://raw.githubusercontent.com/RsyncProject/rsync/master/rsync.1.md`
- 群晖：服务端口列表，
  `https://kb.synology.com/en-uk/DSM/tutorial/What_network_ports_are_used_by_Synology_services`
- QNAP Hybrid Backup Sync：在远端服务器上创建存储空间，
  `https://docs.qnap.com/application/hybrid-backup-sync/3v21.x/en-us/creating-a-storage-space-on-a-remote-server-9B6F1BA0.html`
- TrueNAS：Rsync Tasks 界面，
  `https://www.truenas.com/docs/scale/dataprotection/rsync/rsynctasksscreens/`
