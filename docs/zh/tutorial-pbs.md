# 教程：Proxmox Backup Server 的同步任务

Proxmox Backup Server(PBS)的同步任务把备份从一个 **remote**——另一台 PBS——复制到本地的数据存储。默认情况下，它和 remote 之间只用一条 HTTPS 连接(TLS 上的 HTTP/2，8007 端口)。这篇教程让这条连接经过 tcpraid。

**试过什么，没试过什么。** 这篇教程**没有运行** PBS 本身。实际跑过的是：在一个 TLS 服务器前面放一条转发规则，并核对经规则连过去的客户端看到的证书指纹，和直接连过去的客户端看到的相同。下面的 PBS 命令取自 PBS 的文档(链接在文末)，没有执行过。

**先看看 PBS 自己的选项。** 按它的文档，同步任务有一个 `worker-threads` 选项，可以并行同步多个备份组，每个组用自己的连接。如果你的 PBS 版本有这个选项，而你同步的组又很多，可能有它就够了。tcpraid 针对的是剩下那些仍然只有一条连接的情况：一个很大的备份组，或者没有这个选项的版本。

## 环境

| | 站点 A | 站点 B |
|---|---|---|
| 有 | 存着备份的 PBS，192.0.2.10 的 8007 端口 | 来拉取备份的 PBS |
| tcpraid 节点 | `node-a` | `node-b` |

两个节点已经配对(见 `install-container.md` 或 `install-binary.md`)。

## 第 1 步：在 node-b 上建规则

在 `node-b` 的网页界面里，规则 >“新建规则”：监听地址和端口填 `0.0.0.0:18007`，对端选 `node-a`，目的地填 `192.0.2.10:8007`。或者：

```
node-b$ tcpraid rules add pbs -peer node-a -listen 0.0.0.0:18007 -dest 192.0.2.10:8007
rule pbs added: listening
```

`node-b` 跑在容器里时，用容器映射出来的端口，例如把 18007 换成 50000，这里和下面的命令里都一样。

## 第 2 步：让 remote 指向规则

在拉取备份的那台 PBS 上，remote 的主机和端口改成规则的。指纹仍然是站点 A 那台 PBS 的：tcpraid 原样传递这条 TLS 连接。

```
proxmox-backup-manager remote create pbs-a --host 198.51.100.5 --port 18007 \
    --auth-id sync@pam --password 'SECRET' --fingerprint 64:d3:ff:...:ab:fe
```

这里 `198.51.100.5` 是 `node-b`。对已有的 remote，`proxmox-backup-manager remote update` 有同样的选项。同步任务本身不用改。

实际跑过的检查，用一个测试服务器代替 PBS：客户端直接连和经规则连时看到的指纹。

```
direct:  sha256 Fingerprint=47:3E:78:28:03:9E:08:89:...:CF:15:A3:5D
by rule: sha256 Fingerprint=47:3E:78:28:03:9E:08:89:...:CF:15:A3:5D
```

## 保护了什么

PBS 自己给它的连接加密(TLS)。在两个节点之间，这条连接还会再被包在 tcpraid 自己的 TLS 里。这一段上等于加密了两次；它多花一些 CPU，是稳妥的默认做法。

## 出处

- PBS 文档，Managing Remotes 和同步任务(`remote create` 的例子；`worker-threads`："a single HTTP/2 connection to the source is otherwise bottlenecked by head-of-line blocking")，
  `https://pbs.proxmox.com/docs/managing-remotes.html`
- PBS 文档，`proxmox-backup-manager remote create` 的命令语法(`--host`、`--auth-id`、`--password`、`--fingerprint`、`--port`)，
  `https://pbs.proxmox.com/docs/command-syntax.html`
