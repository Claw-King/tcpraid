# 手动模式

不带子命令时，`tcpraid` 以**手动模式**运行，这时没有节点，没有配对，也没有网页界面。一个进程是 **client**，靠近应用，另一个是 **server**，靠近目的地。一切都由命令行参数或配置文件给出。它是这个程序最底下的一层，适合用在小型路由器上、脚本里，以及你想自己设置传输参数的场合。

```
 app --> [client] ==== data sub-connections (TCP, any path) ====> [server] --> destination
            ^                                                        |
            +------- feedback: UDP / control connection / piggyback -+
```

- client 接受应用的连接，打开子连接，并告诉 server 应用原本要去哪里。它需要能向 server 发起 TCP 连接，直接连，或者经过中继、端口转发或 SOCKS5 代理。server 从不主动连 client。
- server 接受子连接，并在它的允许列表准许的情况下，打开通往目的地的真正连接。

**手动模式不加密数据流。** 见下面的“安全”。

## 快速上手

生成一把密钥，两端用同一个文件：

```
head -c 32 /dev/urandom | base64 > psk
```

server(远端)，只允许访问 192.0.2.5 的 873 端口：

```
tcpraid -mode server -listen 0.0.0.0:9901 -allow 192.0.2.5/32:873 -psk psk
```

client(近端)，转发到一个固定的目的地：

```
tcpraid -mode client -listen 127.0.0.1:9801 -remote server.example.org:9901 \
        -dst 192.0.2.5:873 -psk psk
```

现在让应用连 `127.0.0.1:9801`，而不是 `192.0.2.5:873`。

`tcpraid -check ...` 校验参数和配置文件，然后退出：

```
$ tcpraid -check -mode client -listen 127.0.0.1:9801 -remote server.example.org:9901 -dst 192.0.2.5:873 -psk psk
ok: 0.9.0 client paths [data/data x8], window 128 MB, budget 256 MB, maxrss 0 MB, grace 15m0s, psk true, udp listen "" dial ""
```

## 透明使用

不带 `-dst` 时，client 取每条被重定向过来的连接的原始目的地(Linux 的 `SO_ORIGINAL_DST`，IPv4 和 IPv6)。这种用法只在 Linux 上有。在 macOS 和 Windows 上 client 必须带 `-dst`，否则启动时会报错说明。应用继续连它平常的地址。用 nftables，配在 client 所在的主机上，或者流量经过的路由器上(那里需要 root 权限)：

```
table ip tcpraid {
  chain pre {
    type nat hook prerouting priority dstnat; policy accept;
    iifname "lan0" ip daddr 192.0.2.5 tcp dport 873 redirect to :9801
  }
}
```

`examples/redirect.nft` 还包括了在路由器本机上发起的连接的写法。

## 路径和角色

一个 client 可以同时使用多条路径。路径是连到 server 的一种方式，也就是一个地址，可以经过一个 SOCKS5 代理。每条路径有一个或多个角色：

| 角色 | 承担 | 默认连接数 |
|---|---|---|
| `data` | 新的数据块、重发的数据块、快照 | 8(`subs`) |
| `repair` | 只有重发的数据块；只有当没有任何 `data` 子连接连着时才传新块 | 2(`subs`) |
| `control` | 只有快照 | 1(`ctrl`) |

`data` 和 `repair` 不能放在同一条路径上。典型的配置是一条主路径承担 `data`，另一条独立的路径承担 `repair` 和 `control`。这样缺的块从第二条路径补上，回执不用排在大量数据后面，第一条路径断了的时候由第二条顶上。

路径在配置文件里设置(见下)。命令行参数描述的是常见情形，即一条数据路径(`-remote`、`-n`、`-socks`)，可选地再加控制连接(`-ctrl`、`-ctrlremote`、`-ctrlsocks`)。

想让某条路径走特定的线路而又不需要额外权限，就让它指向一个目的地是 tcpraid server 的代理或端口转发。`mark`(SO_MARK)可以用于策略路由，但需要 CAP_NET_ADMIN，Linux 5.17 起有 CAP_NET_RAW 也可以。它只在 Linux 上有，在别的系统上，带 mark 的路径会在启动时被拒绝。

## 回执通道

在有些线路上，正向忙着传数据时，反向丢包很严重。普通的 TCP 不在乎这一点，因为它的 ACK 是累计的，而且很多，但在那个方向上作为 TCP 数据发送的回执，会卡在一个丢失的报文段后面几秒甚至几十秒。所以 tcpraid 会在你给它的每一条通道上都发送回执，哪一条先到就用哪一条：

| 通道 | 是什么 | 什么时候用 |
|---|---|---|
| UDP | 每份快照一个加密的数据报，从不重传 | 有一端有可达的 UDP 端口时(推荐) |
| `control` 路径 | 一条不承载数据的 TCP 连接，走它自己的路径 | 你有第二条路径，而且数据不会压在它上面 |
| 捎带(piggyback) | 走数据子连接(始终开着；别的通道正常工作时每秒一次) | 兜底 |

用 UDP 时，端口可达的一端监听，另一端发给它。监听的一端按来源地址回复，所以发送的一端可以在 NAT 后面。两端都需要密钥。

```
client: ... -udplisten 0.0.0.0:8445
server: ... -udpdial client.example.org:8445
```

**经验法则：** 至少给 tcpraid 一条不重传的回执通道(UDP)，或者一条反向干净的路径。只有捎带的话，反向丢包严重的线路可能只能跑到一半的速度。

## 配置文件

`-config FILE` 读取一个 YAML 文件。命令行上给出的参数会覆盖它。不认识的键是错误。

```yaml
mode: client
listen: 0.0.0.0:9801
psk_file: /etc/tcpraid/psk
paths:
  - { name: main,   type: tcp, addr: 127.0.0.1:9821, roles: [data], subs: 8 }
  - { name: second, type: tcp, addr: 127.0.0.1:9822, roles: [repair, control], subs: 4 }
  - { name: proxy,  type: socks5, proxy: "socks5://user:pass@127.0.0.1:1080",
      addr: 192.0.2.10:9901, roles: [data], subs: 4 }
control:
  udp: { listen: 0.0.0.0:8445 }
send: { window: 128MB, target: 300ms }
grace: 900s
limits: { maxrss_mb: 700, maxsess: 64 }
```

```
$ tcpraid -config client.yaml -check
ok: 0.9.0 client paths [main/data x8, second/repair x4, second/control x1, proxy/data x4], window 128 MB, budget 256 MB, maxrss 700 MB, grace 15m0s, psk true, udp listen "0.0.0.0:8445" dial ""
```

`addr` 是 client 经这条路径要去连的地址。它是 tcpraid server 的监听，或者通往它的入口。对于目的地固定的端口转发或代理入口，它就是那个转发的地址。server 不需要 `paths`：

```yaml
mode: server
listen: 0.0.0.0:9901
psk_file: /etc/tcpraid/psk
allow: [ "192.0.2.5/32:873" ]
control:
  udp: { dial: client.example.org:8445 }
```

`examples/` 里有手动模式的 systemd 单元、nftables 重定向规则，以及一套双路径配置的 client 和 server 文件。

## 选项

大小在命令行上写纯字节数(`-window 134217728`)，在文件里带单位(`128MB`)。时长用 Go 的写法(`300ms`、`15m`)。

| 参数 | 文件里的键 | 默认值 | 含义 |
|---|---|---|---|
| `-mode` | `mode` | | `client` 或 `server` |
| `-listen` | `listen` | | client：应用连接的地方；server：子连接到达的地方 |
| `-remote` | `paths[].addr` | | client：server 的地址 |
| `-n` | `paths[].subs` | 8 | 每条路径的数据子连接数 |
| `-socks` | `paths[].proxy` | | 经 SOCKS5 代理连到 server |
| `-ctrl`、`-ctrlremote`、`-ctrlsocks` | 角色为 `control` 的 `paths[]` | 0 | 控制连接，以及它们怎样连到 server |
| `-dst` | `dst` | | client：固定的目的地，取代被重定向连接的原始目的地 |
| `-allow` | `allow` | 不限 | server：允许的目的地：`CIDR`、`IPv4-CIDR:port[-port]`、`[IPv6-CIDR]:port[-port]` |
| `-psk` | `psk_file` | 无 | 密钥文件(16 字节或更多)：给报文头和快照做 MAC；用 UDP 时必须有 |
| `-udplisten` | `control.udp.listen` | | 在这里接收 UDP 回执 |
| `-udpdial` | `control.udp.dial` | | 把 UDP 回执发到对端的监听 |
| `-fbsubs` | `control.piggyback_subs` | 3 | 没有控制连接时：每份快照发在几条数据子连接上 |
| `-window` | `send.window` | 128 MB | 每个会话：已发送而尚未被报告收到的数据量；也是提供给对端的乱序缓存空间 |
| `-budget` | `send.budget` | 256 MB | 所有会话合计：已发送而未确认的数据，以及(另算)持有的乱序数据 |
| `-target` | `send.target` | 300 ms | 每条子连接在它的回执周期之外还可以排多少队 |
| `-pigtarget` | `send.stalled_target` | 1 s | 回执卡住之后的 10 秒内，`-target` 改用这个值 |
| `-hol` | `send.hol` | 1 s | 一个缺失的块可以挡住后面的块多久，之后就重发它 |
| `-chunk` | `send.chunk` | 256 KB | 数据块大小 |
| `-grace` | `grace` | 900 s | 对端沉默这么久之内，保留会话 |
| `-dead` | `dead` | 20 s | 子连接上有在途数据却这么久没有进展，就换掉它 |
| `-kickratio` | | 5 | 一条忙碌的子连接，交付速度连续 10 秒低于所在路径中位数除以这个值，就换掉它 |
| `-maxrss` | `limits.maxrss_mb` | 关闭 | 超过这么多 MB 就退出；至少为 2 × `budget` + 100 MB(启动时检查)。只在 Linux 上有：在 macOS 和 Windows 上启动时会被拒绝，那里的配置文件里不要写 `limits.maxrss_mb` |
| `-maxsess` | `limits.maxsess` | 64 | 超过这个数目就拒绝新会话 |
| `-timeline` | `timeline` | 关闭 | 每隔这么久为每个会话记一行诊断信息 |
| `-stats` | | 关闭 | 每隔这么久记录会话、内存和每条子连接的状态 |
| `-metrics` | `metrics` | 关闭 | 在 `http://ADDR/debug/vars` 提供 expvar 指标 |
| `-check` | | | 校验参数和文件，输出结果，退出 |
| `-version` | | | 输出版本和线路协议版本 |

其余的调节参数和测试参数见 `tcpraid -h`。

**内存。** 最坏情况下一个进程持有大约两倍 `budget` 的流数据，默认值下约 512 MB。在小机器上请调低 `budget` 和 `window`。

## 运行

**停止。** 遇到 SIGINT / SIGTERM、`-maxrss` 或 `-life` 时，tcpraid 先通知每一个对端，然后重置它的应用连接并退出。对端会立刻丢弃这些会话，而不是连同缓存的数据一起保留 `grace` 那么久。

**重启。** 会话状态在内存里。重启之后的进程没法恢复它的会话，所以应用会看到连接被重置，必须重试。

**可观测性。** `-timeline 1s` 为每个会话记录：已发送(`tx`)、重发(`re`)、已确认(`ack`)、已接收(`rx`)和已交付(`dlv`)的 MB/s，未确认和乱序的积压，回执的新旧程度以及它是从哪些通道到达的(`fbc` 控制连接，`fbu` UDP)，达到上限的子连接数，等待缺失块的最老的块等了多久(`holage`)，读取方等待窗口的时间占比(`pumpwait`)，以及回执周期的 p50 / p95(毫秒，`loop`)。最先暴露回执问题的两个数字是 `pumpwait` 和回执周期的 p95。`troubleshooting.md` 逐个解释了这一行的每个字段，以及会话结束时记的那两行。

## 安全

- 手动模式**不加密数据流**。如果数据需要保护，让它走一条已经加密的路径，或者使用节点(`tcpraid run`)，节点默认加密。
- 没有 `-psk` 时，任何能连到 server 端口的人都可以向允许的目的地发起连接。务必设置 `-allow`，并在任何你不完全信任的路径上使用密钥。
- 有密钥时，子连接的报文头和快照会被认证(HMAC-SHA256，截断到 16 字节)，超过 10 分钟的报文头会被拒绝。两端的时钟相差必须在 10 分钟之内。
- UDP 回执是加密并认证的(AES-256-GCM，每个会话、每个方向一把密钥)。认证失败的数据报会被丢弃，处理它们的频率也有上限。

## 兼容性

client 和 server 必须运行相同的线路协议版本，没有协商。节点和手动模式的进程之间不能互通，因为节点之间的连接以一段开场白开头，而手动模式不发送它。
