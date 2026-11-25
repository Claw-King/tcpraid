# Using a node from a Go program

The package `github.com/Claw-King/tcpraid/client` lets a Go program work
with a tcpraid node that runs on the same host. It talks to the node's local
API, like the `tcpraid` commands, which are built on it.

The package may still change before version 1.0 of tcpraid. A change is
named in the release notes of the version that brings it.

## What it needs

A node that runs, and its data directory. The node keeps there the address
of its API, the access token and, when the API is served over TLS, the
certificate it presents. The program must run as a user who can read that
directory. Over TLS the package talks only to an API that holds the key of
the recorded certificate, and sends nothing to any other.

Whoever can read the token can do everything the API offers. So a program
that uses the package has the same power over the node as the `tcpraid`
command.

## Reading and changing

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

| Method | What it does |
|---|---|
| `Status`, `Sessions`, `Peers`, `Peer`, `Rules`, `Transport` | read the node's status, its live sessions, its peers, its rules and its transport settings |
| `AddRule`, `ReplaceRule`, `RemoveRule` | make, replace and remove a rule |
| `UpdatePeer`, `SetPath`, `RemovePath` | change a peer's paths, allow list, UDP address and transport settings |
| `CheckPeer`, `TestPeer` | try the paths to a peer, and measure them with test data |
| `SetTransport` | set transport settings of the node while it runs |
| `Call`, `Raw` | any other request of the API (`reference-api.md`) |

The types of the requests and the answers are the ones `reference-api.md`
describes, under the same names as the JSON has them.

An error tells three cases apart. `errors.Is(err, client.ErrNoNode)` means
that the node could not be asked: it does not run, or the directory is not
its data directory. `errors.Is(err, client.ErrTimeout)` means that the node
did not answer within `Timeout`. An error of the type `*client.Error` is
what the node answered, with the HTTP status and the reason.

The data directory is read for every request, so a `Client` goes on working
when the node is started again.

## A connection to a destination at a peer

```go
conn, err := c.Dial(ctx, "nas-b", "192.0.2.10:5432")
if err != nil {
	return err
}
defer conn.Close()
```

`Dial` returns a `net.Conn` to `host:port` as seen from the peer. The node
carries it as one session, like the connection of a rule, and the peer holds
the destination against the allow list it keeps for this node. The host can
be a name, which the peer looks up.

A `*client.RefusedError` means that the node was asked and no connection
began. Its message is the reason: the peer is not connected, the allow list,
the destination, the name.

The connection keeps the rule that a stream which was cut off is not taken
for a complete one, with the exceptions that the README lists under "Known
limits":

- `Read` returns `io.EOF` only when the destination ended its side in order.
  A connection that was cut off gives another error.
- `CloseWrite` ends the direction to the destination. The destination then
  reads the end of a complete stream, and reading from it goes on.
- `Close` before `CloseWrite` resets the connection. The destination sees a
  reset and does not take what it got for all there was. After `CloseWrite`,
  `Close` is an ordinary close.

So a program that sends a request and reads an answer writes, calls
`CloseWrite` when the protocol wants the end of the request to be seen,
reads to `io.EOF`, and closes. A program that gives up in the middle just
closes.

A `Write` or a `CloseWrite` that returned without an error says that the
node took the data, which the node may still hold. Since version 0.11.0 the
method `Done` says where it got to. Call it after `CloseWrite` and after
`Read` has returned `io.EOF`:

```go
if err := conn.Done(ctx); err != nil {
	// the destination may not have all that was sent
}
```

`Done` waits until the node has ended the session, about a second after the
two directions have ended, and returns nil when the stream ended completely
in both directions. The other node then wrote everything to its connection
to the destination and ended that direction in order. Whether the
destination's program read it is beyond what a node can see. A `*CutError`
with the node's reason means that the stream was cut off before that. An
error that matches `ErrNoWord` means that the node cannot say, because it
runs a version before 0.11.0. A node that no longer knows the session
answers with an `*Error` of status 404, which is no word that the stream
ended complete. A node keeps how its last sessions ended for a few minutes.

One limit is shared with `tcpraid connect` (`reference-cli.md`). With a peer
of a version before 0.11.0 the node says that a connection began before it
can be sure that the destination took it, so a destination that does not
answer at all shows as a connection that begins and is then cut off.
