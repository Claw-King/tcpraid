# 在 Go 程序里使用节点

`github.com/Claw-King/tcpraid/client` 这个包让 Go 程序可以和同一台主机上运行的 tcpraid 节点打交道。它走的是节点的本机 API，和 `tcpraid` 的各条命令一样，那些命令就是在它上面写的。

在 tcpraid 1.0 版之前，这个包还可能改动。改动会写在带来它的那个版本的发布说明里。

## 需要什么

一个在运行的节点，以及它的数据目录。节点把 API 的地址、访问令牌放在那里，API 用 TLS 提供时还有它出示的证书。程序必须以读得到这个目录的用户身份运行。用 TLS 时，这个包只和持有那张证书的密钥的 API 通信，对别的什么都不发。

读得到令牌的人，API 能做的都能做。所以使用这个包的程序，对节点的权力和 `tcpraid` 命令一样大。

## 读取和修改

```go
c := client.New("/var/lib/tcpraid")
c.Timeout = 10 * time.Second

st, err := c.Status(ctx)
if err != nil {
	return err
}
fmt.Println(st.Node.Name, st.Version)

rules, err := c.Rules(ctx)
```

| 方法 | 做什么 |
|---|---|
| `Status`、`Sessions`、`Peers`、`Peer`、`Rules`、`Transport` | 读节点的状态、在跑的会话、对端、规则和传输层设置 |
| `AddRule`、`ReplaceRule`、`RemoveRule` | 新建、替换、删除一条规则 |
| `UpdatePeer`、`SetPath`、`RemovePath` | 修改对端的路径、允许列表、UDP 地址和传输层设置 |
| `CheckPeer`、`TestPeer` | 试一遍到对端的各条路径，以及用测试数据测量它们 |
| `SetTransport` | 在节点运行中设置它的传输层设置 |
| `Call`、`Raw` | API 的其他任何请求(`reference-api.md`) |

请求和回答的类型就是 `reference-api.md` 里讲的那些，名字和 JSON 里的相同。

错误分三种情况。`errors.Is(err, client.ErrNoNode)` 表示问不到节点：它没有在运行，或者这个目录不是它的数据目录。`errors.Is(err, client.ErrTimeout)` 表示节点没有在 `Timeout` 之内回答。类型是 `*client.Error` 的错误是节点的回答，带着 HTTP 状态和原因。

每个请求都重新读数据目录，所以节点重新启动之后，`Client` 照样能用。

## 到对端那边某个目的地的连接

```go
conn, err := c.Dial(ctx, "nas-b", "192.0.2.10:5432")
if err != nil {
	return err
}
defer conn.Close()
```

`Dial` 返回一条 `net.Conn`，通到从对端看过去的 `host:port`。节点把它当作一个会话来承载，和规则的连接一样，对端会拿目的地去对它为本节点保存的允许列表。主机可以是名字，由对端解析。

`*client.RefusedError` 表示问到了节点，但连接没有开始。它的消息就是原因：对端没有连着、允许列表、目的地、名字。

这条连接守着同一条规矩，被切断的数据流不会被当成完整的，例外列在 README 的“已知限制”里：

- `Read` 只有在目的地按顺序结束了它那一侧时才返回 `io.EOF`。被切断的连接返回的是别的错误。
- `CloseWrite` 结束通往目的地的那个方向。目的地随后读到一个完整数据流的结束，而从它那里读数据照常进行。
- 在 `CloseWrite` 之前调用 `Close` 会重置连接。目的地看到的是重置，不会把已经收到的当成全部。在 `CloseWrite` 之后，`Close` 是普通的关闭。

所以发一个请求再读回答的程序，先写，在协议要对方看到请求结束的地方调用 `CloseWrite`，读到 `io.EOF`，然后关闭。中途放弃的程序直接关闭就行。

`Write` 或 `CloseWrite` 不带错误地返回，说的是节点收下了数据，而这些数据可能还在节点里。从 0.11.0 版起，方法 `Done` 回答它们到了哪里。在 `CloseWrite` 之后、`Read` 返回 `io.EOF` 之后调用它：

```go
if err := conn.Done(ctx); err != nil {
	// the destination may not have all that was sent
}
```

`Done` 等到节点结束这个会话，这比两个方向结束大约晚一秒。数据流在两个方向上都完整结束时它返回 nil，这时另一个节点已经把全部数据写进了它通往目的地的连接，并按顺序结束了那个方向。目的地的程序有没有读，节点看不到，README 的“已知限制”里还写了一种目的地没有全部收到的情况。`*CutError` 带着节点给的原因，表示数据流在这之前被切断了。与 `ErrNoWord` 相符的错误表示节点答不上来，因为它的版本早于 0.11.0。节点已经不认得这个会话时，回答是状态 404 的 `*Error`，这不算数据流完整结束的凭据。节点把最近结束的会话各是怎么结束的记几分钟。

有一条限制和 `tcpraid connect` 的相同(`reference-cli.md`)。对端的版本早于 0.11.0 时，节点在能确定目的地接下连接之前就说连接开始了，所以完全没有回应的目的地，看起来是一条开始之后被切断的连接。
