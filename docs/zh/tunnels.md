# 和隧道、组网工具或代理一起使用 tcpraid

通往对端的路径，就是一个可以向它发起 TCP 连接的地址。这个地址不一定是对端自己的，它可以是任何能把 TCP 送到对端监听的东西在近端的入口。这份文档讲怎么配置。选哪种连法、哪些工具能让 tcpraid 的连接在路上保持各自独立，见 `connecting.md`，因为不是所有工具都能做到。

## 规则

- **路径的地址**是拨出节点为了连到对端的监听而去连的地址。它可以是端口转发的地址、隧道本地一端的地址，或者对端在组网工具里的地址。对 SOCKS5 代理来说，它是代理应该去连的地址。
- **路径的档位**决定 tcpraid 是否在这条路径上加自己的 TLS。默认的 `tls` 加，`off` 不加。

两者都在拨出节点上设置，可以在网页界面的 对端 > 详情 > 那条路径 > 编辑 里设，也可以用 `tcpraid peers path set`。

## 端口转发，或者远端固定的隧道

有个东西在本节点附近监听，并把连接送到对端的 9851 端口，例如路由器上转发的端口，或者把连接转交过去的中继。让路径指向近端：

```
nas-b$ tcpraid peers path set nas-a default -addr 127.0.0.1:19851
```

档位保持 `tls`，所以两个节点仍然互相认证并端到端加密，包在承载这条连接的东西里面。

这里不要用 SSH 端口转发(`ssh -L`、`ssh -R`)。它能用，但它转发的每一条连接都是同一条 SSH 连接里的一个通道，于是 tcpraid 的所有连接共用一条 TCP 连接，得不到任何好处。任何会复用连接的转发工具都是这样。`connecting.md` 列出了我们查证过的几个。

## 组网工具或 VPN

两个节点在组网里各有一个地址。把对端在组网里的地址作为路径的地址，并在生成配对码时把它填作“对方用什么地址连到本节点”。别的都不用改。

## 已经加密的路径：`off` 档

如果隧道是从节点到节点全程加密的，tcpraid 自己的 TLS 就是第二层。要把它去掉：

1. 启动监听节点时，再加一个明文监听：
   `tcpraid run ... -listen-plain 0.0.0.0:9852`。务必保证这个端口**只能**经隧道连到。
2. 在拨出节点上，把路径切换过去：

   ```
   nas-b$ tcpraid peers path set nas-a default -security off -addr 127.0.0.1:19852
   path default of nas-a changed
   PATH     TYPE  ADDRESS          PROXY  SECURITY  VERIFY  PIN  ROLES  SUBS
   default  tcp   127.0.0.1:19852  -      off       -       -    data   8

   nas-b$ tcpraid peers check nas-a
   default  127.0.0.1:19852 (off): ok
   ```

在 `off` 档下，数据不由 tcpraid 加密。证明连接者是谁的，只有一段简短的、经过认证的开场白，能读到这条路径的人可以在十分钟内重放它，详见 `security.md`。只在隧道覆盖了两个节点之间全程的地方使用它。

配对本身始终需要 TLS 监听(`-listen`)，一个节点可以同时开两种监听。不拨出的节点上的规则经 `off` 路径也能用。这一点实际跑过，当时一个节点只开明文监听，这个节点上的一条规则把 60 MiB 送到了另一个节点，校验值相同。

## 经 SOCKS5 代理

路径可以经过拨出节点附近的一个 SOCKS5 代理。这时路径的地址是代理要去连的地址：

```
nas-b$ tcpraid peers path set nas-a side -addr 192.0.2.10:9851 -type socks5 \
           -proxy socks5://user:pass@127.0.0.1:1080 -roles repair,control -subs 2
```

写在命令行上的代理密码会出现在进程列表里。网页界面也可以填，而且填过之后不会再显示。经代理的路径不在为这套文档所做的实际运行范围内。

## 多条路径

一个对端可以有多条路径，走不同的线路，承担不同的角色，详见 `concepts.md`。常见的形态是主线路承担 `data`，另一条独立的线路承担 `repair` 和 `control`。

```
nas-b$ tcpraid peers show nas-a
name         nas-a
fingerprint  e5645f9b28e9434c67a7c434c15142e742bd9459afae8fc979e1640f7795ec86
paired       2026-01-15T10:00:00Z
addresses    nas-a.example.org:9851
state        online, runs 0.12.0, last heard 2026-01-15T10:00:05Z
features     path-test, resolve, rule-paths, dial-both, spare, held, session-ok

PATH     TYPE    ADDRESS                 PROXY                    SECURITY  VERIFY  PIN  ROLES           SUBS
default  tcp     nas-a.example.org:9851  -                        tls       -       -    data            8
side     socks5  192.0.2.10:9851         socks5://127.0.0.1:1080  tls       -       -    repair,control  2
```

`tcpraid peers path remove NAME PATH` 删除一条路径。`tcpraid peers check NAME` 逐条试每条路径，并说明哪些不通。

## 你自己的证书：`owncert` 档

如果监听节点应该出示你的证书而不是它的身份证书，例如由签发机构针对某个主机名签发的证书，启动它时在 `-listen` 之外加上 `-cert FILE -key FILE`，并在拨出节点上设置路径：

```
# checked by the fingerprint of the certificate's key (the listening node logs it at start)
tcpraid peers path set nas-a default -security owncert -pin FINGERPRINT

# or checked by authority and host name, as a web client would
tcpraid peers path set nas-a default -security owncert -verify ca -addr nas-a.example.org:9851
```

拨出节点仍然出示它的身份证书，开场白也仍然证明配对关系。这一档由程序的测试覆盖，但不在为这套文档所做的实际运行范围内。
