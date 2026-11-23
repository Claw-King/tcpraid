# 两个站点怎么连

规则能承载数据之前，得先有一个节点能连到另一个节点。这份文档讲这需要什么、几种办法按什么顺序去试，以及哪些连法会让 tcpraid 无事可做。一条路径的各项设置在 `tunnels.md` 里讲得更细，这份文档讲的是怎么选连法。

## tcpraid 需要什么

tcpraid 有两个要求，公网 IP 地址只是满足第一个要求的办法之一。

**一个节点能向另一个节点的监听发起 TCP 连接。** 一个方向就够，一个端口也就够，也就是监听节点的 `-listen` 端口(例子里是 9851)。两个节点里由哪一个监听都行。规则建在哪一个节点上都能用，因为连不到对端的节点会请对端把连接拨过来。被连的地址也不必是监听节点自己的。它可以是路由器转发的一个端口、节点在组网工具里的地址，或者第三台机器上的一个端口，由这台机器把连接转交过去，下文把它叫作转发主机。所以某一个站点有公网地址，只是做到这一点的办法之一。

**这些连接在路上保持各自独立。** 一个会话由好几条 TCP 连接承载，一条路径上默认是 8 条。tcpraid 的好处，来自每一条连接各自处理自己丢的包。如果路上有什么东西把它们全都并进一条连接里，它们就共用这条连接对丢包的反应，tcpraid 就不比单独一条连接多出多少，甚至一点也不多。这条连接是 TCP 连接的话，丢一个包会让所有连接一起停下来。数据仍然会到，而且仍然是对的，只是不会更快，也不会更稳。各种工具的区别就在这里，下面的表说明哪个是哪种。

## 按什么顺序试

### 1. 一个站点有 IPv6

没有公网 IPv4 地址的站点，可能仍然有公网的 IPv6 地址，所以先看一下运行节点的机器有哪些地址。如果一个站点有，而另一个站点能向外发起 IPv6 连接，就不需要第三台机器，也不需要别的程序。

- 在监听节点所在站点的路由器上加一条入站规则，放行到节点 IPv6 地址 9851 端口的 TCP。IPv6 不做地址转换，所以这条规则只需要放行这个端口。
- 启动监听节点时用能接受 IPv6 的监听，并告诉它对方该用哪个地址：

  ```
  nas-b# tcpraid run -listen '[::]:9851' -advertise '[2001:db8:1::10]:9851'
  ```

- 照常配对，在 `nas-b` 上执行 `tcpraid pair create`，在 `nas-a` 上执行 `tcpraid pair join`。`nas-a` 得到的路径就指向这个地址：

  ```
  nas-a$ tcpraid peers check nas-b
  default  [2001:db8:1::10]:9851 (tls): ok
  ```

要用这台机器上不会变的地址，不要用临时地址，或者用一个跟着地址走的主机名。`-advertise` 和路径的地址既可以写地址，也可以写名字。数据不经过任何第三台机器。

### 2. 能让两台机器直接相连的组网工具

在两台机器上装同一个组网工具，比如 Tailscale、ZeroTier，或者自己搭的 WireGuard 隧道。之后每台机器在组网里都有一个地址。生成配对码时给出监听节点在组网里的地址：

```
nas-b$ tcpraid pair create -addr 198.51.100.20:9851
```

这里的 `198.51.100.20` 代表 `nas-b` 在组网里的地址。依靠它之前先看两件事：

- **组网工具确实让两台机器直接相连**，而没有退到中继上。下面的表说明各个工具怎么显示这一点。哪次传输比以前慢了，再看一遍。
- **组网本身有多快。** 直接相连时，这些工具用 UDP 数据报承载流量。先用你顺手的任何工具量一下两台机器之间组网本身的速度，再把 tcpraid 放上去。tcpraid 的速度不会超过它所依托的组网。

路径的档位保持 `tls`，除非你有理由改。什么时候适合用 `off`，见 `tunnels.md`。

### 3. 一台有公网地址的机器，把连接转交过去

如果上面两种都不行，可以租一台有公网地址的机器放在中间，当转发主机。监听节点所在的站点保持有东西连着转发主机，另一个节点则去连转发主机的一个端口。

- **frp**：照它的文档配置，让节点的 9851 端口成为转发主机上的一个端口。在 `frps.toml` 和 `frpc.toml` **两边**都写上 `transport.tcpMux = false`。按默认值(`true`)，frp 的文档说，到同一个 `frpc` 的所有连接会被复用到同一条 TCP 连接里，这正是 tcpraid 无事可做的那种情况。`transport.protocol` 保持默认的 `tcp`。
- **从每个站点各拉一条 WireGuard 隧道到转发主机**，由转发主机在两条隧道之间转发。WireGuard 用 UDP 承载 IP 包，所以 tcpraid 的每一条连接从节点到节点仍然是一条 TCP 连接。
- 只在转发主机上转发一个端口是不够的，除非转发主机自己能连到监听节点，比如经过节点所在站点保持着的一条通往它的隧道。

这时拨出节点的路径指向转发主机。配对时就给这个地址，在监听节点上执行 `tcpraid pair create -addr vps.example.org:19851`，或者配对之后再设：

```
nas-a$ tcpraid peers path set nas-b default -addr vps.example.org:19851
```

这种连法的代价：

- 每一个字节都要进转发主机、再从它出去。它的带宽就是上限。有流量配额的话，整次传输都要算进去，进出都计的话算两遍。
- 档位保持默认的 `tls`。这样两个节点之间是从一头加密到另一头，转发主机转交的是它读不了的数据。

## 逐个工具

这里只列出我们在它们自己的文档里查得到行为的工具。别的工具，用下一节的办法检查。

| 连法 | 连接是否保持独立？ | 要设什么，看什么 |
|---|---|---|
| 公网地址，或者路由器转发的端口 | 是 | 不用设 |
| frp | 只有在 `frps.toml` 和 `frpc.toml` 里都写了 `transport.tcpMux = false` 时才是 | 默认是 `true`，这时所有连接共用 `frpc` 和 `frps` 之间的一条 TCP 连接 |
| SSH 端口转发(`ssh -L`、`ssh -R`) | 否 | 每一条被转发的连接都是同一条 SSH 连接里的一个通道。不要用它做路径 |
| WireGuard | 是 | 它用 UDP 承载 IP 包，里面的每条 TCP 连接各自处理自己丢的包。先量一下隧道本身的速度 |
| Tailscale | 两台机器直接相连时是 | 两台机器之间有过流量之后，`tailscale status`(或 `tailscale ping`)里对方那一行显示 `direct`。显示 `relay` 说明流量走的是 Tailscale 的 DERP 中继服务器，Tailscale 的文档说它的服务质量有限，所以不要在上面跑大量传输。`peer-relay` 是另一种，中继在你自己网络里的一台机器上，见 Tailscale 的文档 |
| ZeroTier | 两台机器直接相连时是 | `zerotier-cli peers` 里对方那一行显示 `RELAY`，说明两台机器没能直接相连，它的包经过 ZeroTier 的根服务器(ZeroTier 文档的示例里，直接相连时显示的是 `DIRECT`)。`zerotier-cli info` 显示 `TUNNELED` 说明它退到了 TCP 中继上，ZeroTier 的文档说那个中继慢 |
| Cloudflare Tunnel | 否 | `cloudflared` 和 Cloudflare 的网络之间走的是它自己的 QUIC 或 HTTP/2 连接，经它转发的连接不是各自一条 TCP 连接。不要用它做路径 |

## 怎么看出连接是不是各自独立

`tcpraid sessions` 在 `SUBS` 一栏显示节点为一个会话持有几条连接。那是节点开出去的数量，不是在两个站点之间实际走的数量。想看得更多，就在传输进行中数 TCP 连接。

在监听节点上：

```
nas-b$ ss -Htn state established '( sport = :9851 )' | wc -l
9
```

在只有 BusyBox 的系统上用 `netstat -tn | grep -c ':9851 .*ESTABLISHED'`。两个节点直接相连、而且只有一次传输在进行时，这个数是会话的 `SUBS` 加一，多出来的一条是两个节点之间保持着的控制连接。有几个会话时，是它们的和再加一。到这里为止是我们实际跑过的。

有转发主机时，监听节点看到的仍然是每条子连接各一条连接，来自转交它们的那个东西。用 frp 时那是节点自己这个站点里的 `frpc`，用隧道时是对端节点在隧道里的地址。要紧的是两个站点之间的那一段，所以要到转发主机上去数从另一个站点来的、或者去往另一个站点的 TCP 连接有几条。按道理，连接是一条一条转交的话，数出来应当和节点上差不多，被复用了的话只有一条。我们没有在上面每一个工具上都这样跑过。

用 UDP 承载包的隧道，路上没有 TCP 连接可数。这种情况看工具自己显示的状态，是直连还是中继，见上表。

## 出处

这份文档里关于其他程序的说法，都出自它们自己的文档。默认值会随版本变化，请以你所用版本的文档为准。

- frp，README 的 “TCP Stream Multiplexing” 一节：
  https://raw.githubusercontent.com/fatedier/frp/dev/README.md
  以及示例配置：
  https://raw.githubusercontent.com/fatedier/frp/dev/conf/frpc_full_example.toml
- SSH 连接协议，RFC 4254(通道；TCP/IP 端口转发)：
  https://www.rfc-editor.org/rfc/rfc4254.txt
  以及 SSH 传输层协议，RFC 4253(它跑在 TCP 之上)：
  https://www.rfc-editor.org/rfc/rfc4253.txt
- WireGuard：https://www.wireguard.com/ 和
  https://www.wireguard.com/known-limitations/
- Tailscale，“Connection types”：
  https://tailscale.com/docs/reference/connection-types
- ZeroTier，“TCP Relay”：https://docs.zerotier.com/relay/ ，
  命令行：https://docs.zerotier.com/cli/ ，
  以及 “Relaying”：https://docs.zerotier.com/faq/relaying/
- Cloudflare Tunnel，“Tunnel run parameters”：
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/
  以及 “Useful terms”：
  https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/tunnel-useful-terms/
- QUIC(那条连接不是 TCP 的情况)：RFC 9000(流)和 RFC 9002(一条路径一个拥塞控制)：
  https://www.rfc-editor.org/rfc/rfc9000.txt
  https://www.rfc-editor.org/rfc/rfc9002.txt
