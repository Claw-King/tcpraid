English: [README.md](README.md)

# tcpraid

tcpraid 把**一条 TCP 连接**拆到**多条 TCP 连接**上传输，在对端按原来的顺序重新拼好。应用程序不用改，也察觉不到。

它是给这样的软件用的：做大量传输时只开一条 TCP 连接——存储复制、备份流、数据库复制——而线路又长或者有丢包，单条连接又慢又不稳，多条并行却跑得很好。

名字借用了 RAID 的想法：几个普通的成员合在一起，比单独一个更快、更稳健。

状态：0.1，第一个公开版本。线路协议版本 1(见 `PROTOCOL.md`)，在 1.0 之前仍可能变动。依赖它之前，请先读下面的“已知限制”。

## 它适合你吗？

下面几条都成立时，它有帮助：

- 你有一条长时间的大流量数据流，它自己没法拆成多条并行连接。这里有教程的例子：TrueNAS 上的 ZFS 复制、rsync 守护进程、PostgreSQL 流复制、Proxmox Backup Server 的同步任务。
- 在你的线路上，多条并行的 TCP 连接合起来明显比一条更快或更稳。(快速试一下：用网络测速工具 `iperf3`，分别跑一条流和八条流。)
- 两端你都能运行 tcpraid。

除了速度，它还带来：

- **断线不会让数据流中断。** 所有子连接都断了的时候，两端会把应用的连接保持最长 15 分钟，线路恢复后接着传。
- **被截断的数据流不会被当成完整的。** 异常结束时，两边应用的连接都会被重置。(只剩一种情况：节点停止时的一条空连接，见“已知限制”。)
- **两个节点之间默认加密**(TLS 1.3)；每个节点都会把对方的密钥和配对时记下的进行核对。

这些情况下它帮不上忙：

- 一条 TCP 连接已经能跑满线路。
- 线路又短又干净(局域网、邻近的数据中心)。
- 应用自己就能开并行连接。那就用它自己的。

它不是 VPN，也不是通用代理：它只承载你为之建了规则的那些 TCP 连接，而且只在你自己运行的两台机器之间。

## 怎么用

在每个站点运行一个**节点**——装在 NAS 本机上、容器里，或者装在为局域网服务的路由器或网关上。把两个节点**配对**一次：在一个节点上生成配对码，复制到另一个节点上。之后在任意一个节点上建**转发规则**：“到达这个端口的连接，送到另一个站点的那个地址”。

```
 application --> [ node B ] ===== several TCP connections =====> [ node A ] --> destination
                  rule: port 9801 -> 192.0.2.10:873 at node A's site
```

两个节点里只要有一个能被另一个连到就行。所有操作都可以在节点的网页界面里完成，也可以用 `tcpraid` 命令完成。

## 五分钟上手

两台机器：`nas-a`(另一台可以通过 `nas-a.example.org` 连到它)和 `nas-b`。用容器，两台上都用 `deploy/docker-compose.yml`：

1. **在每台机器上启动一个节点。** 先在 compose 文件里把 `hostname:` 改成这个节点的名字——一台写 `nas-a`，另一台写 `nas-b`：节点第一次启动时用它作为自己的名字，两个名字必须不同。然后 `docker compose up -d`，再 `docker logs tcpraid`：日志里会显示网页界面的密码，只显示这一次。
2. **打开每个节点的网页界面**：`http://NAS-的地址:9850/`。
3. **把它们配对。**(如果第 1 步没有设名字，现在设，要在配对之前：设置 >“本节点的名字”。节点记住的是对端在配对那一刻的名字。)在 `nas-a` 上：对端 >“生成配对码”；在“对方用什么地址连到本节点”里填 `nas-a.example.org:9851`，点“生成”；复制配对码。在 `nas-b` 上：对端 >“输入配对码”；粘贴进去。现在两边的页面都显示对方在线。
4. **建一条规则。** 在 `nas-b` 上：规则 >“新建规则”；监听地址和端口填 `0.0.0.0:50000`，对端选 `nas-a`，目的地填 `192.0.2.10:873`(从 `nas-a` 看过去的地址)。
5. **用起来。** 让应用连 `nas-b` 的 50000 端口，而不是 `192.0.2.10` 的 873 端口。

同样的事用命令来做(这里是在容器里执行；不用容器时去掉 `docker exec ... /` 和 `-data /data`)：

```
nas-a$ docker exec tcpraid /tcpraid pair create -data /data -addr nas-a.example.org:9851
tcpraid-pair-1-iiph6pbe4iey...
nas-b$ docker exec -i tcpraid /tcpraid pair join -data /data
tcpraid-pair-1-iiph6pbe4iey...        <- pasted; then Enter and Ctrl-D
paired with nas-a (fingerprint 056f8829...e2e077d8)
nas-b$ docker exec tcpraid /tcpraid rules add rsync -data /data -peer nas-a \
           -listen 0.0.0.0:50000 -dest 192.0.2.10:873
rule rsync added: listening
```

`docs/zh/install-container.md` 和 `docs/zh/install-binary.md` 会把这个过程慢慢讲一遍。

## 要求

- **两端都是 Linux**(amd64、arm64 或 32 位 ARM v7)，用容器或者一个静态二进制文件。
- **每一端都要有一个节点。** 远端只有一台普通的服务器是不够的。
- **两个节点里有一个能向另一个发起 TCP 连接**，直接连，或者经过端口转发、隧道或 SOCKS5 代理。
- **只承载应用的 TCP 流量。**
- **内存。** 最坏情况下一个节点会缓存约 512 MB 的流数据。
- **两端的时钟相差不超过 10 分钟。**

## 文档

| | |
|---|---|
| `docs/zh/concepts.md` | 节点、配对、对端、路径和角色、加密档位、规则、传输是怎么工作的 |
| `docs/zh/install-container.md` | 用容器运行节点：compose、TrueNAS、群晖、QNAP、Unraid |
| `docs/zh/install-binary.md` | 用二进制文件运行节点，配合 systemd |
| `docs/zh/tutorial-truenas-zfs-replication.md` | **两台 TrueNAS 之间的 ZFS 复制** |
| `docs/zh/tutorial-rsync.md` | 用 rsync 连到 rsync 守护进程 |
| `docs/zh/tutorial-postgresql.md` | PostgreSQL 流复制 |
| `docs/zh/tutorial-pbs.md` | Proxmox Backup Server 的同步任务 |
| `docs/zh/security.md` | 保护了什么、没保护什么、配对码或文件泄露意味着什么 |
| `docs/zh/tunnels.md` | 和隧道、组网工具或代理一起使用 |
| `docs/zh/troubleshooting.md` | 不通或者很慢的时候看什么 |
| `docs/zh/reference-cli.md` | 每一条命令 |
| `docs/zh/reference-api.md` | 本机 HTTP API，带示例 |
| `docs/zh/reference-files.md` | 数据目录和 `tcpraid run` 的选项 |
| `docs/zh/manual-mode.md` | 不用节点的模式：一个 client、一个 server，用命令行参数或配置文件 |
| `PROTOCOL.md` | 线路协议(只有英文) |
| `deploy/README.md` | 构建脚本、镜像、容器模板(只有英文) |

## 已知限制

这是第一个版本，由一个开发者用业余时间做成。已经知道的：

- **只支持 Linux。** 其他系统不支持。
- **只在很少的机器上测过。** 教程是在容器里跑的，用普通程序代替 NAS；没有任何一步在任何品牌的真实 NAS 上试过。amd64 的二进制文件构建出来了，但作者从未运行过；Unraid 和 QNAP 的容器模板只对照公开的格式核对过。
- **网页界面用得很少。** 它由脚本渲染并点击过一遍，没有被人实际使用过。
- **规则的目的地是 IP 地址**，不能写主机名。
- **停止节点时，恰好有目的地还在连接中。** 如果在停止一个节点的那一刻，有一个刚开始的会话正在这个节点上连它的目的地，目的地可能看到一条刚打开就又关上、没有任何数据的连接，像一条空的数据流。
- **会话撑不过任何一个节点的重启**；应用会看到连接被重置，需要重试。
- **两个节点里必须有一个能被另一个连到**；不经第三台机器中转。持有通往对端路径的节点，在这条路径不通时不会退而请对端连过来。
- **网页界面和本机 API 是明文 HTTP。** 请在你信任的网络里使用，或者放在带 TLS 的反向代理后面。
- **有人占着大量连接不放时，监听端口能限制它造成的影响，但不能完全杜绝**(见 `docs/zh/security.md`)。
- **两个节点必须运行相同的线路协议版本**；目前还没有协商机制。
- **节点之间经 UDP 的回执、经 SOCKS5 的路径、`owncert` 档**在程序的测试里是通的，但不在教程的实际运行范围内。

## 实际使用的结果

来自作者自己的使用：用的是这个程序的前身，手动模式，一条约 240 毫秒的洲际线路，负载是存储复制：

- 一条 205 GB 的数据流平均 85 MB/s；约 4% 的数据块被重发过。
- 一条 7 TiB 的数据流跑了五天。其间它的数据通路中断过一次，持续 10 分钟；会话保持住了，之后接着传。

这些是在一条线路上的单次测量，不是基准测试，而且不是用这个版本测的。

## 构建和测试

```
go build -trimpath -o tcpraid ./cmd/tcpraid      # Go 1.27 or newer
go vet ./... && go test -race -count=1 ./...
```

`deploy/build.sh` 构建发布用的二进制文件；`deploy/Dockerfile` 构建镜像。

## 许可证

Apache License 2.0。见 `LICENSE` 和 `NOTICE`。
