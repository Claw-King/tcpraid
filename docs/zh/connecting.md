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

## 同时用不止一种连法

上面的几种连法可以同时配上两种，一种不通时由另一种接手。

**一条路径写多个地址。** 一条路径可以写同一个监听的最多 8 个地址，拨出节点每次打开连接时按顺序尝试。把想优先用的地址写在前面，比如先写 IPv6 地址，再写转发主机：

```
nas-a$ tcpraid peers path set nas-b default -addr [2001:db8:1::10]:9851,vps.example.org:19851
```

带了多个地址的配对码，会把这些地址全部交给新建的路径。一个不应答的地址，会让每条去试它的连接最多等 8 秒，直到有一个地址连通为止。之后节点从连通的那个地址开始试。`tcpraid peers check` 会显示每个地址的情况。一条路径的所有地址共用这条路径的设置，所以它们必须通向同一个监听，档位也相同。正在跑的会话继续用它已有的连接。

**两个方向各一条路径。** 两个站点都能被对方连到时，给每个节点都配一条通往对方的路径。配对时没有得到路径的节点，用 `tcpraid peers path set` 加上。平时每个节点照旧自己打开自己规则的连接。当一个节点没法经自己的路径打开连接、而另一个节点还连着它时，新会话的连接改由另一个节点打开。它自己的路径恢复之后，节点会自己改回来。

节点靠它在这条路径上保持的控制连接知道路径断了。到对端有几条路径时，看的是带控制角色的第一条路径，都没有这个角色就看第一条，别的路径不看。这条控制连接断开、并且重新连不上之后，路径才算不通。路径上的连接被切断、新的连接被拒绝时，最多十秒左右就能发现。路径不再传数据、但也不拒绝连接时，最长要一分钟才发现，这段时间里到来的连接会等 30 秒，然后被重置。正在跑的会话不会被挪到另一个方向。原来走这条路径的会话会等它恢复，和只有一条路径的节点上一样。带 `-paths` 或 `-subs` 的规则只用本节点自己的路径。

**指定由哪个节点拨出。** 规则可以用 `-dial` 指定连接由哪个节点打开(见 `reference-cli.md`)。`self` 和 `peer` 把这件事定死。`both` 让两个节点为同一个会话各自在自己的路径上打开子连接。两个方向是两条不同的线路时这样做有用，比如一个站点有 IPv6 地址，另一个站点有一个转发过来的端口。这时会话同时用两条线路，其中一条断了，它靠另一条继续跑。两个方向走的是同一条线路时，`both` 只是让会话在这条线路上多一倍的子连接，没有别的作用。

`both` 要求两个节点各有一条通往对方的路径，并且都是 0.6.0 或更新的版本。两条路径缺了一条时，规则的连接被重置，规则的状态里写明缺的是哪一条。路径配了但眼下不通是另一回事。这时会话先靠另一个节点的子连接跑起来。不通的是对端的路径时，对端每 20 秒被再请求一次。不通的是规则所在节点自己的路径时，这个节点会一直重拨。缺的子连接到来之后，会话就用上两边的。

## 备用连接

由对端打开连接的会话，比节点自己拨的会话起步晚。请求要先经控制连接发过去，然后对端连过来，两个节点完成认证，之后节点才能发出会话的头。在长距离线路上，这几步要好几个来回。

所以没有路径通往对端的节点会请对端预先备好几条连接。它们在任何会话之前就打开并完成认证，然后空着，等会话来用。新会话立刻在其中一条上起步，其余的子连接照原来的方式请求。对端随后再打开一条备用连接，补上用掉的那条。

两个节点都要是 0.6.0 或更新的版本，各自按自己的 `-spares` 办(见 `reference-files.md`)。没有路径的节点要这么多条，拨出节点最多保持这么多条，默认是 2。任何一个节点设成 `-spares 0` 就关掉了。备用连接走第一条承载数据的路径，带 `-paths` 或 `-subs` 的规则不用它们。节点只接受它开口要过的对端打开的备用连接，而且这条连接要先通过两个节点之间每条连接都要过的那道认证。`tcpraid peers show` 会显示有几条在等。

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

先测一下。`tcpraid peers test` 在每条路径上发测试数据，一次只用一条子连接，一次用全部，然后把两个速度都打印出来：

```
nas-a$ tcpraid peers test nas-b
path test with nas-b: two runs of up to 10 seconds for each path
PATH     SUB-CONNECTIONS  ONE ALONE  ALL TOGETHER  TIMES
default  8                2.6 MB/s   19.8 MB/s     7.6
default: 8 sub-connections together carried 7.6 times what one carried alone.
```

全部合起来并不比一条多多少的时候，有两种解释，这项测试分辨不出是哪一种：

- **一条连接已经占满了线路。** 把 `ONE ALONE` 和你知道的这条线路的速度比一比。两者接近，就没有出问题。在这样的线路上，单条连接本来就跑得好，tcpraid 没有多少可以加的。
- **路径上有东西把这几条连接合进了一条。** 这时 `ONE ALONE` 明显低于线路的速度。对照上面的表查你用的工具，再按下面的办法数一数连接。

这项测试发的是真实的数据，只在你运行它的时候进行。它的参数见 `reference-cli.md`。

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
