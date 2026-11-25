# 用二进制文件运行节点

这份文档适用于普通的 Linux 主机、路由器或网关，也适用于 Mac 和 Windows 机器。tcpraid 是一个二进制文件，没有运行时依赖，在 Linux 上是静态的。下面的步骤按 Linux 写，后面“在 macOS 和 Windows 上”一节说明那两个系统上有什么不同。

## 获取二进制文件

**下载。** `https://github.com/Claw-King/tcpraid/releases` 上的每个版本都有各个系统的二进制文件，和一个记着它们 SHA-256 校验值的文件。Linux 用的是：

| 机器(`uname -m`) | 文件 |
|---|---|
| `x86_64` | `tcpraid-0.11.1-linux-amd64` |
| `aarch64` | `tcpraid-0.11.1-linux-arm64` |
| `armv7l` | `tcpraid-0.11.1-linux-armv7` |
| | `tcpraid-0.11.1-SHA256SUMS` |

把适合你机器的文件和校验文件下载到同一个目录，用校验文件核对二进制文件，然后安装：

```
$ curl -LO https://github.com/Claw-King/tcpraid/releases/download/v0.11.1/tcpraid-0.11.1-linux-arm64
$ curl -LO https://github.com/Claw-King/tcpraid/releases/download/v0.11.1/tcpraid-0.11.1-SHA256SUMS
$ grep linux-arm64 tcpraid-0.11.1-SHA256SUMS | sha256sum -c
tcpraid-0.11.1-linux-arm64: OK
# install -m 0755 tcpraid-0.11.1-linux-arm64 /usr/local/bin/tcpraid

$ tcpraid version
tcpraid 0.11.1 (wire protocol 1)
```

你下载的文件所在的一行如果不是 `OK`，说明下载到的不是发布的文件，不要运行它。

**或者自己构建**(Go 1.27 或更新)：

```
go build -trimpath -o tcpraid ./cmd/tcpraid
```

或者用 `deploy/build.sh 0.11.1` 构建全部发布用的二进制文件，脚本开头写了没装 Go 时怎么做。把适合你机器的文件复制到 `/usr/local/bin/tcpraid`。用上面那条普通的 `go build` 得到的二进制文件报告的是 `0.11.1-dev`，而构建脚本会把给它的版本号写进去。

## 在 macOS 和 Windows 上

同一个程序也能在 macOS 和 Windows 上作为节点运行，并且能和三种系统上的节点互相配对。这两个系统是新加的。到目前为止，程序在它们上面经过了仓库的自动测试和一次手动试用，试用时一台 Mac 和一台 Windows 机器互相配对。详见 README 的“已知限制”。

| 系统 | 文件 |
|---|---|
| macOS，Apple 芯片 | `tcpraid-0.11.1-macos-arm64` |
| macOS，Intel | `tcpraid-0.11.1-macos-amd64` |
| Windows，x64 | `tcpraid-0.11.1-windows-amd64.exe` |
| Windows，ARM | `tcpraid-0.11.1-windows-arm64.exe` |

**核对下载到的文件。** 在 macOS 上，打开终端，进入放着这两个文件的目录：

```
$ grep macos-arm64 tcpraid-0.11.1-SHA256SUMS | shasum -a 256 -c
tcpraid-0.11.1-macos-arm64: OK
$ chmod +x tcpraid-0.11.1-macos-arm64
$ ./tcpraid-0.11.1-macos-arm64 version
```

在 Windows 上，用 PowerShell，用眼睛比对两个值。除了字母大小写可以不同，它们必须相同：

```
> (Get-FileHash .\tcpraid-0.11.1-windows-amd64.exe -Algorithm SHA256).Hash
> Select-String windows-amd64 .\tcpraid-0.11.1-SHA256SUMS
> .\tcpraid-0.11.1-windows-amd64.exe version
```

**这些二进制文件没有签名。** 第一次启动时，系统可能会拦下它，说无法确认这个程序的来源。如果你已经核对过 SHA-256 校验值，就按系统的提示或它的安全设置里提供的办法允许它运行。在我们知道的那一次里，用 `scp` 拷到 Mac 上、在终端里启动的二进制文件没有被拦下。用浏览器下载的文件，系统可能另作处理，那种情况没有试过。节点要接受别的机器连进来时(`-listen`，或者规则的监听端口)，系统还可能询问是否允许这个程序接受传入的连接。macOS 问了，按钮是 Deny 和 Allow。要允许，否则另一个节点连不上。

**运行**在终端里进行。tcpraid 的各个命令和参数与 Linux 上相同，只是把 `tcpraid` 换成你下载的文件的名字，在 PowerShell 里写成 `.\tcpraid-0.11.1-windows-amd64.exe`。这份文档里围绕这些命令的 shell 写法是按 Linux 写的，我们没有在这两个系统上试过。不同的地方：

- **数据目录。** 不带 `-data` 时，节点把数据放在当前用户的配置目录下名为 `tcpraid` 的目录里。macOS 上是 `~/Library/Application Support/tcpraid`，Windows 上是 `%AppData%\tcpraid`。向节点发请求的各个命令(`status`、`pair`、`peers`、`rules`、`sessions`)以同一个用户运行时，也去那里找。
- **只在 Linux 上有的：** `-transparent` 和透明规则，`-maxrss`，`-stats` 那一行里的 `rss`(别的系统上显示为 `n/a`)，容器镜像，systemd 单元。在别的系统上，`tcpraid run` 启动时会拒绝 `-transparent` 和 `-maxrss`。
- **让它一直运行。** 终端开着，程序就运行着。关闭终端之前先按 Ctrl+C 停掉它，因为直接关窗口会让程序立刻结束，来不及通知另一个节点。想让它随系统启动，就用系统自己提供的办法，比如 macOS 的 `launchd`，或者 Windows 的计划任务或服务封装工具。我们没有为这两个系统提供安装程序，也没有提供服务定义。
- **机器睡眠时，上面的节点在这段时间里是停着的。** 睡眠期间传输不动，另一个节点通往这个节点的连接断开满 15 分钟(`-grace`)，就会放弃会话。节点经历睡眠会怎样，不论长短，我们都没有试过。

## 启动节点

手动启动，先试一下：

```
nas-a# tcpraid run -name nas-a -listen 0.0.0.0:9851 -advertise nas-a.example.org:9851
... web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
... web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
... node 0.11.1 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
... transport window=128MB budget=256MB grace=15m0s hol=1s timeline=off maxrss=off
... web interface: open http://127.0.0.1:9850 in a browser on this machine
```

- `-name` 是节点的名字，对端和对端的规则用它来指这个节点。不给的话，新节点的名字是主机名加上四个随机字符，例如 `nas-a-k3x9`。
- `-listen` 是其他节点连接的地方。在能被另一个节点连到的节点上给出，另一个节点不需要。两个站点都没有公网地址的话，`connecting.md` 把让一个节点能被连到的几种办法逐一讲了。
- `-advertise` 是另一个节点连它时要用的地址。它会被写进这个节点生成的配对码里。
- 数据目录是 `/var/lib/tcpraid`，除非用 `-data` 另外指定。里面有节点的私钥，所以要保留好，像备份私钥那样备份它。
- 网页界面和 API 在 `127.0.0.1:9850`，只有本机能访问。要从别的机器打开页面，加上 `-api ADDRESS:9850`。它是明文 HTTP，所以只能在你信任的网络上这样做，或者再加上 `-api-tls`，让节点用它自己生成的证书经 TLS 提供页面(见 `security.md`)。

用 systemd 的话，`deploy/tcpraid-node.service` 就是为此准备的单元文件。它让节点以一个专用的用户运行，没有 root 的权限：

```
# useradd --system --user-group --no-create-home --shell /usr/sbin/nologin tcpraid
# cp deploy/tcpraid-node.service /etc/systemd/system/
```

然后调整单元文件里的命令行。加上 `-name NAME` 给节点起你想要的名字。在另一个节点要连过来的节点上，还要加上 `-advertise HOST:9851`，因为 `tcpraid pair create` 会把这个地址写进配对码，没有的话每次都要给 `-addr`。然后执行 `systemctl enable --now tcpraid-node`。生成的密码在 `journalctl -u tcpraid-node` 里。

- systemd 会创建 `/var/lib/tcpraid`，并把它交给用户 `tcpraid`。
- 命令行里提到的文件，比如 `-cert`、`-key`、`-ui-password-file` 指向的文件，要让这个用户能读。
- 1024 以下的端口需要一项权限，单元文件里有两行是为它准备的，默认注释掉了。转发规则和透明规则不需要别的权限。
- 如果节点以前是用这个单元文件的旧版本以 root 运行的，先停掉它，建好用户，换上新的单元文件，再启动。数据目录留在原处，systemd 会在这次启动时把它交给新用户。

```
nas-a$ tcpraid status
node         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
version      0.11.1 (wire protocol 1)
uptime       1s
sessions     0
peers        0
rules        0
```

这些命令通过数据目录和同一台主机上的节点通信，所以要用能读这个目录的用户来执行(用这个单元文件时，就是 root 或用户 `tcpraid`)。节点没有用默认目录时，加上 `-data DIR`。

## 把两个节点配对

```
nas-a$ tcpraid pair create
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node with "tcpraid pair join", which then reads the code from standard input.
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

这里表格里的指纹缩短了。

## 建一条规则

在发起传输的应用旁边的节点上：

```
nas-b$ tcpraid rules add rsync -peer nas-a -listen 0.0.0.0:9801 -dest 192.0.2.10:873
rule rsync added: listening

nas-b$ tcpraid rules
NAME   TAKES         PEER   DESTINATION     SESSIONS  STATE
rsync  0.0.0.0:9801  nas-a  192.0.2.10:873  0         listening
```

连到 `nas-b` 的 9801 端口的连接，现在会从 `nas-a` 那一头出来，去往 `192.0.2.10:873`，这是一个从 `nas-a` 看过去的地址。

在 `nas-a` 上建一条通往 `nas-b` 的规则也可以用，虽然 `nas-a` 没有通往 `nas-b` 的路径。这时它会请 `nas-b` 来打开连接，详见 `concepts.md`。

## 在路由器上：透明规则

装在站点的路由器或网关上的节点，可以在应用毫不知情的情况下承载连接。应用继续用原来的地址，由路由器把连接重定向到节点。

1. 启动节点时开一个透明监听：
   `tcpraid run -transparent 0.0.0.0:9800 ...`
2. 把连接重定向到它。tcpraid 不会改主机的包过滤规则，所以这条规则由你来加。用 nftables，针对从局域网接口进来、经路由器转发的连接：

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

   `-match` 接受网段，可带端口或端口段，用逗号分隔，例如 `192.0.2.0/24:873,192.0.2.0/24:50000-50010`。

第一条匹配的透明规则接走连接，被重定向过来、却没有任何规则匹配的连接会被重置。连接在对端那一头会去往同一个地址，所以这个地址在对端那边必须指的是同一台机器。

写这份文档时，实际把本机发起的连接(`output` 钩子)重定向到了一个带透明规则的节点，经它传了 20 MiB，两端校验值相同。上面的 `prerouting` 写法用于经路由器转发的连接，就是 `examples/redirect.nft` 里的写法。0.1.0 版的第一位用户在路由器上跑过它，负载是一个存储复制任务。
