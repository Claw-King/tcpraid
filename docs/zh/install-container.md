# 用容器运行节点

在 NAS 上运行 tcpraid 就用这种方式。两个站点各需要一个节点。这里的步骤是在一台 Linux 机器上用 Docker 跑的。没有任何一步在真实的 NAS 上试过(见“各平台怎么用”)。

## 容器需要什么

| | |
|---|---|
| 镜像 | `ghcr.io/claw-king/tcpraid:0.6.0`(也可以用 `deploy/Dockerfile` 自己构建) |
| 数据 | 挂载到 `/data` 的一个目录。里面有节点的私钥、它的对端和它的规则。保留好，像备份私钥那样备份它，绝不要让两个节点共用 |
| 端口 9850 | 网页界面和本机 API。明文 HTTP：只在你信任的网络上开放 |
| 端口 9851 | 其他节点连到这里。能被另一个节点连到的那个节点需要它；另一个节点开着也无妨 |
| 端口 50000-50010 | 给转发规则用。在网页界面里建的规则必须监听你映射出来的端口之一，地址用 `0.0.0.0` |
| 端口 8446，UDP | 可选：对端经 UDP 发来的回执。节点启动时带了 `-udp-listen` 才会用到 |
| `TCPRAID_UI_PASSWORD` | 可选：网页界面的密码。不设的话，节点会生成一个，并在日志里显示一次 |

`deploy/docker-compose.yml` 里这些都有，并带注释。给规则用的端口段只是默认值。要改的话，把映射两边改成相同的端口即可。文件里还给出了另一种做法，就是不映射端口，而是共用主机的网络。这样规则可以监听主机上任何空闲的端口。

UDP 回执默认不启用。要用它，就在节点的命令行里加上 `-udp-listen 0.0.0.0:8446`，把 8446 端口按 UDP 映射出来，再在另一个节点上用 `tcpraid peers set NAME -udp-dial HOST:8446` 告诉它这个地址。compose 文件里这两行都有，默认注释掉了。

**共用主机网络时，要写明网页界面在哪里监听。** 镜像启动节点时用的是 `-api 0.0.0.0:9850`，在端口映射后面这是对的。共用主机网络时，它会把网页界面和 API(明文 HTTP)开在主机的每一个地址上，包括朝向别的网络的地址，比如在路由器或网关上。这种情况下给容器写上它自己的命令，指明页面该开在哪个地址，可以只开在主机本机，也可以开在局域网的某一个地址：

```
    network_mode: host
    command: ["run", "-data", "/data", "-api", "127.0.0.1:9850", "-listen", "0.0.0.0:9851"]
```

用 `127.0.0.1` 时，页面只能从主机本机打开。从别的机器打开要经过一条隧道，例如 `ssh -L 9850:127.0.0.1:9850 HOST`。

## 启动

```
$ docker compose up -d
$ docker logs tcpraid
... web interface: no password was set, so one was made. It is shown this once: y7ygtkha76gshsnt
... web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
... node 0.6.0 name="nas-a" fingerprint=056f8829...e2e077d8 data=/data api=[::]:9850 listen=[::]:9851 listen-plain=off transparent=off udp-listen=off
... transport window=128MB budget=256MB grace=15m0s hol=1s timeline=off maxrss=off
... web interface: open http://127.0.0.1:9850 in a browser on this machine
... the web interface and the API at [::]:9850 are plain HTTP and reachable from other hosts. Use them on a network you trust, or put a reverse proxy with TLS in front.
```

打开 `http://主机的地址:9850/`，用那个密码登录。

**配对之前先给节点起名字。** 节点第一次启动时用容器的主机名加上四个随机字符作为自己的名字。compose 文件不改的话，得到的是 `tcpraid-k3x9` 这样的名字，每台机器上都不一样。在 设置 >“本节点的名字”里给每个节点起一个认得出的名字。本页用的是 `nas-a` 和 `nas-b`。要在配对之前做，因为每个节点记下的是对端在那一刻的名字，规则靠这个名字指明对端，之后再改名传不到另一个节点。

## 把两个节点配对

其中一个节点的 9851 端口必须能被另一个连到。把它叫做 `nas-a`，可以通过 `nas-a.example.org` 连到，另一个叫 `nas-b`。两个站点都没有公网地址的话，先看 `connecting.md`。

在网页界面里：

1. 在 `nas-a` 上，打开 对端 >“生成配对码”。在“对方用什么地址连到本节点”里填 `nas-a.example.org:9851`。容器里的节点不知道自己对外的地址，所以要告诉它。点“生成”，然后复制配对码。它只显示一次、只能用一次，有效期一小时。
2. 在 `nas-b` 上，打开 对端 >“输入配对码”，粘贴配对码，点“配对”。
3. 现在两边的页面都列出了对方，几秒之内显示为在线。在 `nas-b` 上，对端的“详情”里有一个“检查连接”按钮。

也可以用命令。在容器里程序是 `/tcpraid`，数据目录是 `/data`：

```
nas-a$ docker exec tcpraid /tcpraid pair create -data /data -addr nas-a.example.org:9851
pairing code a101905f, valid until Thu, 15 Jan 2026 11:00:00 UTC, for one use.
Enter it on the other node with "tcpraid pair join", which then reads the code from standard input.
It is shown this once and is a secret until it is used.
tcpraid-pair-1-iiph6pbe4iey...

nas-b$ docker exec -i tcpraid /tcpraid pair join -data /data
tcpraid-pair-1-iiph6pbe4iey...        <- pasted; then Enter and Ctrl-D
paired with nas-a (fingerprint 056f8829...e2e077d8)

nas-b$ docker exec tcpraid /tcpraid peers -data /data
NAME   STATE   FINGERPRINT           PAIRED                PATHS
nas-a  online  056f8829...e2e077d8   2026-01-15T10:00:00Z  nas-a.example.org:9851 (tls, data)

nas-a$ docker exec tcpraid /tcpraid peers -data /data
NAME   STATE   FINGERPRINT           PAIRED                PATHS
nas-b  online  36dc0793...0fe0ca7f   2026-01-15T10:00:00Z  none: this node cannot reach it
```

指纹是 64 位十六进制，这里缩短了。不带 `-addr` 时，容器里的 `pair create` 会回答：

```
tcpraid pair create: no address for the other node to connect to. Name one in the request, or start the node with -advertise HOST:PORT.
```

`nas-a` 上显示 "none: this node cannot reach it" 是正常的。`nas-b` 保持着一条通往 `nas-a` 的连接，`nas-a` 上的规则用它来请 `nas-b` 连过来，详见 `concepts.md` 的“由哪个节点发起连接”。

## 建一条规则

在发起传输的应用旁边的节点上，比如 `nas-b`，打开 规则 >“新建规则”：

- 名字：随便起，例如 `rsync`。
- 监听地址和端口：`0.0.0.0:50000`(你映射出来的一个端口)。
- 对端：`nas-a`。
- 对端那边的目的地址和端口：另一个站点的服务，按 `nas-a` 连它的方式写，例如 `192.0.2.10:873`。也可以写主机名，例如 `nas.lan:873`，它由 `nas-a` 在每条连接到来时解析。

或者：

```
nas-b$ docker exec tcpraid /tcpraid rules add rsync -data /data -peer nas-a -listen 0.0.0.0:50000 -dest 192.0.2.10:873
rule rsync added: listening
```

现在让应用连 `nas-b` 的地址、50000 端口，而不是 `192.0.2.10` 的 873 端口。各篇教程会针对具体的应用演示这一步。

要从另一台 NAS 自己的容器里访问那台 NAS **本机**上的服务，目的地要写 NAS 的局域网地址，不要写 `127.0.0.1`，因为在容器里 `127.0.0.1` 是容器自己。

## 各平台怎么用

| 平台 | 做法 |
|---|---|
| TrueNAS(24.10 或更新) | Apps > Discover Apps > Install via YAML，用 `deploy/docker-compose.yml` |
| 群晖 | Container Manager > Project，用 compose 文件 |
| QNAP | Container Station > Applications > Create，用 compose 文件；或者用自定义模板 `deploy/qnap/tcpraid-templates.json` |
| Unraid | 容器模板 `deploy/unraid/tcpraid.xml` |
| 任何装了 Docker 的 Linux | `docker compose up -d` |

这些文件的说明在 `deploy/README.md`(只有英文)。以上都没有在真实的产品上试过。菜单名称来自各厂商的文档，模板只对照公开的格式核对过。在 NAS 上，数据目录请用数据集或共享文件夹上的绝对路径。

## 更新

换掉镜像，用同一个数据目录重新启动容器。节点会保留它的身份、它的对端和它的规则。正在运行的会话会被重置，应用需要重试。
