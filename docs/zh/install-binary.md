# 用二进制文件运行节点

适用于普通的 Linux 主机、路由器或网关。tcpraid 是一个静态二进制文件，没有运行时依赖。

## 获取二进制文件

**下载。** `https://github.com/Claw-King/tcpraid/releases` 上的每个版本都有三个二进制文件，和一个记着它们 SHA-256 校验值的文件：

| 机器(`uname -m`) | 文件 |
|---|---|
| `x86_64` | `tcpraid-0.1.3-linux-amd64` |
| `aarch64` | `tcpraid-0.1.3-linux-arm64` |
| `armv7l` | `tcpraid-0.1.3-linux-armv7` |
| | `tcpraid-0.1.3-SHA256SUMS` |

把适合你机器的那一个和校验文件下载到同一个目录，用校验文件核对二进制文件，然后安装：

```
$ curl -LO https://github.com/Claw-King/tcpraid/releases/download/v0.1.3/tcpraid-0.1.3-linux-arm64
$ curl -LO https://github.com/Claw-King/tcpraid/releases/download/v0.1.3/tcpraid-0.1.3-SHA256SUMS
$ grep linux-arm64 tcpraid-0.1.3-SHA256SUMS | sha256sum -c
tcpraid-0.1.3-linux-arm64: OK
# install -m 0755 tcpraid-0.1.3-linux-arm64 /usr/local/bin/tcpraid

$ tcpraid version
tcpraid 0.1.3 (wire protocol 1)
```

你那个文件的那一行如果不是 `OK`，说明下载到的不是发布的那个文件：不要运行它。

**或者自己构建**(Go 1.27 或更新)：

```
go build -trimpath -o tcpraid ./cmd/tcpraid
```

或者用 `deploy/build.sh 0.1.3` 构建三个发布用的二进制文件；脚本开头写了没装 Go 时怎么做。把适合你机器的那一个复制到 `/usr/local/bin/tcpraid`。(用那条普通的 `go build` 得到的二进制文件报告的是 `0.1.3-dev`；构建脚本会把给它的版本号写进去。)

## 启动节点

手动启动，先试一下：

```
nas-a# tcpraid run -listen 0.0.0.0:9851 -advertise nas-a.example.org:9851
... web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
... web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
... node 0.1.3 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
... transport window=128MB budget=256MB grace=15m0s hol=1s timeline=off maxrss=off
```

- `-listen` 是其他节点连接的地方。在能被另一个节点连到的那个节点上给出；另一个节点不需要。两个站点都没有公网地址的话，`connecting.md` 把让一个节点能被连到的几种办法逐一讲了。
- `-advertise` 是另一个节点连它时要用的地址。它会被写进这个节点生成的配对码里。
- 数据目录是 `/var/lib/tcpraid`，除非用 `-data` 另外指定。里面有节点的私钥：保留好，像备份私钥那样备份它。
- 网页界面和 API 在 `127.0.0.1:9850`，只有本机能访问。要从别的机器打开页面，加上 `-api ADDRESS:9850`——它是明文 HTTP，所以只能在你信任的网络上这样做。

用 systemd：`deploy/tcpraid-node.service` 就是为此准备的单元文件。把它复制到 `/etc/systemd/system/`，调整里面的命令行(在另一个节点要连过来的那个节点上，加上 `-advertise HOST:9851`：`tcpraid pair create` 会把这个地址写进配对码，没有的话每次都要给 `-addr`)，然后 `systemctl enable --now tcpraid-node`。生成的密码在 `journalctl -u tcpraid-node` 里。

```
nas-a$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.1.3 (wire protocol 1)
uptime       1s
sessions     0
peers        0
rules        0
```

这些命令通过数据目录和同一台主机上的节点通信，所以要用能读这个目录的用户来执行(单元文件不改的话就是 root)；节点没有用默认目录时，加上 `-data DIR`。

## 把两个节点配对

```
nas-a$ tcpraid pair create
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node: tcpraid pair join (it then reads the code from standard input)
It is shown this once and is a secret until it is used.
tcpraid-pair-1-ueazax3dhku3...

nas-b$ tcpraid pair join
tcpraid-pair-1-ueazax3dhku3...        <- pasted; then Enter and Ctrl-D
paired with nas-a (fingerprint e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86)
```

`pair join` 从标准输入读配对码，所以它不会出现在进程列表里，也不会留在 shell 的历史里。现在两个节点互相列出了对方：

```
nas-b$ tcpraid peers
NAME   STATE   FINGERPRINT           PAIRED                PATHS
nas-a  online  e5645f9b...7795ec86   2026-01-15T10:00:00Z  nas-a.example.org:9851 (tls, data)

nas-b$ tcpraid peers check nas-a
default  nas-a.example.org:9851 (tls): ok
```

(这里表格里的指纹缩短了。)

## 建一条规则

在发起传输的应用旁边的那个节点上：

```
nas-b$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:9801 -dest 192.0.2.10:873
rule rsync added: listening

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:9801  nas-a  192.0.2.10:873  0         listening
```

连到 `nas-b` 的 9801 端口的连接，现在会从 `nas-a` 那一头出来，去往 `192.0.2.10:873`——一个从 `nas-a` 看过去的地址。

在 `nas-a` 上建一条通往 `nas-b` 的规则也可以用，虽然 `nas-a` 没有通往 `nas-b` 的路径：它会请 `nas-b` 来打开连接(见 `concepts.md`)。

## 在路由器上：透明规则

装在站点的路由器或网关上的节点，可以在应用毫不知情的情况下承载连接：应用继续用原来的地址，由路由器把连接重定向到节点。

1. 启动节点时开一个透明监听：
   `tcpraid run -transparent 0.0.0.0:9800 ...`
2. 把连接重定向到它。tcpraid 不会改主机的包过滤规则；这条规则由你来加。用 nftables，针对从局域网接口进来、经路由器转发的连接：

   ```
   table ip tcpraid {
     chain pre {
       type nat hook prerouting priority dstnat; policy accept;
       iifname "lan0" ip daddr 192.0.2.10 tcp dport 873 redirect to :9800
     }
   }
   ```

   `examples/redirect.nft` 还包括了在路由器本机上发起的连接的写法。这需要路由器上的 root 权限。
3. 建一条透明规则，说明哪些原始目的地送到哪个对端：

   ```
   gw-b$ tcpraid rules add site-a -peer gw-a -match 192.0.2.10/32:873
   rule site-a added: listening

   gw-b$ tcpraid rules
   NAME    TAKES                            PEER  DESTINATION      SESSIONS  STATE
   site-a  redirected to 192.0.2.10/32:873  gw-a  (as redirected)  0         listening
   ```

   `-match` 接受网段，可带端口或端口段，用逗号分隔：`192.0.2.0/24:873,192.0.2.0/24:50000-50010`。

第一条匹配的透明规则接走连接；被重定向过来、却没有任何规则匹配的连接会被重置。连接在对端那一头会去往同一个地址，所以那个地址在那边必须指的是同一台机器。

写这份文档时实际跑过的：把本机发起的连接(`output` 钩子)重定向到一个带透明规则的节点，经它传了 20 MiB，两端校验值相同。上面那种 `prerouting` 写法，用于经路由器转发的连接，就是 `examples/redirect.nft` 里的那种；0.1.0 版的第一位用户在路由器上跑过它，负载是一个存储复制任务。
