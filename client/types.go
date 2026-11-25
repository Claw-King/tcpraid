package client

// The requests and the answers of the local API, as JSON has them.

import "time"

// Paths of the API.
const (
	PathHealth       = "/healthz"
	PathStatus       = "/api/v1/status"
	PathSessions     = "/api/v1/sessions"
	PathMetrics      = "/metrics"
	PathPairingCodes = "/api/v1/pairing-codes"
	PathPeers        = "/api/v1/peers"
	PathRules        = "/api/v1/rules"
	PathTransport    = "/api/v1/transport"
	PathConnect      = "/api/v1/connect"
	PathNode         = "/api/v1/node"
)

// Status is the answer of GET /api/v1/status.
type Status struct {
	Version       string     `json:"version"`
	WireVersion   int        `json:"wire_version"`
	Started       time.Time  `json:"started"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	Node          StatusNode `json:"node"`
	Sessions      int        `json:"sessions"`
	Peers         int        `json:"peers"`
	Rules         int        `json:"rules"`
	// Since 0.8.0. Listen: the addresses at which the node takes connections of other
	// nodes with TLS, as bound (an address like 0.0.0.0:9851 stands for every address
	// of the host); empty when the node cannot be joined. Advertise: the addresses the
	// node puts into a pairing code that is asked for without any.
	Listen    []string `json:"listen"`
	Advertise []string `json:"advertise"`
	// Since 0.10.0. Metrics: the address at which the node serves its figures without
	// a token, as bound; not there for a node that was started without -metrics.
	Metrics string `json:"metrics,omitempty"`
}

// StatusNode names the node in a Status.
type StatusNode struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// Session is one element of the answer of GET /api/v1/sessions.
type Session struct {
	ID           string    `json:"id"` // hex
	Destination  string    `json:"destination"`
	Role         string    `json:"role"` // "client" or "server"
	Started      time.Time `json:"started"`
	TxBytes      int64     `json:"tx_bytes"`
	RxBytes      int64     `json:"rx_bytes"`
	UnackedBytes int64     `json:"unacked_bytes"`
	ResentBytes  int64     `json:"resent_bytes"`   // stream bytes queued for sending again
	Subs         int       `json:"subs"`           // live sub-connections
	SubsDialed   int       `json:"subs_dialed"`    // of those, the ones this node opened (since 0.6.0); the peer opened the rest
	Peer         string    `json:"peer,omitempty"` // the peer at the other end
	Rule         string    `json:"rule,omitempty"` // role client: the rule that carries the connection
	// Since 0.3.0. unacked_bytes is the sum of the next two: read from the application
	// and never sent yet, and sent but not yet reported received by the peer.
	UnsentBytes   int64 `json:"unsent_bytes"`
	InflightBytes int64 `json:"inflight_bytes"`
	// Up: the peer has answered on a sub-connection at least once.
	Up bool `json:"up"`
	// SilentMs is how long ago anything arrived from the peer. GraceLeftMs is how much
	// longer the session lasts if nothing arrives; at zero it is reset.
	SilentMs    int64 `json:"silent_ms"`
	GraceLeftMs int64 `json:"grace_left_ms"`
	// Test: the session carries the data of a path test, not an application connection.
	Test bool `json:"test,omitempty"`
	// WindowBytes (since 0.7.0) is the send window the session was created with at
	// this node: the node's, or the one its peer or its rule sets.
	WindowBytes int64 `json:"window_bytes"`
}

// PairingCodeRequest is the body of POST /api/v1/pairing-codes. Both fields are
// optional: without addresses the node uses the ones it was started with, without a
// time to live the default.
type PairingCodeRequest struct {
	Addresses  []string `json:"addresses"`
	TTLSeconds int64    `json:"ttl_seconds"`
}

// PairingCode describes a pairing code. Code is the code itself and is set only in the
// answer to the request that made it.
type PairingCode struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
	Used    bool      `json:"used"`
	Code    string    `json:"code,omitempty"`
}

// JoinRequest is the body of POST /api/v1/peers. Address is optional: where to connect
// instead of the addresses in the code.
type JoinRequest struct {
	Code    string `json:"code"`
	Address string `json:"address"`
}

// Peer describes a peer. It has no pair secret.
type Peer struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	Addresses   []string  `json:"addresses"`
	PairedAt    time.Time `json:"paired_at"`
	Paths       []Path    `json:"paths"`
	Allow       string    `json:"allow"`    // destinations the peer may reach through this node; empty: any
	UDPDial     string    `json:"udp_dial"` // where UDP feedback for the peer is sent; empty: nowhere
	// What the control connections with the peer say.
	Online      bool       `json:"online"`                 // one is up, in either direction
	Version     string     `json:"version,omitempty"`      // the program version the peer reported
	LastMessage *time.Time `json:"last_message,omitempty"` // when the last message from the peer arrived
	// Spares (since 0.6.0) is the number of spare connections between the two nodes
	// that wait for a session to begin on them; absent when there is none.
	Spares int `json:"spares,omitempty"`
	// Features (since 0.11.0) is what the peer said it can do when the control
	// connection came up, by the names of PROTOCOL.md; absent before that.
	Features []string `json:"features,omitempty"`
	// Problem (since 0.11.0) says why no connection is carried to or from the peer
	// although it is online; absent when there is no such reason.
	Problem string `json:"problem,omitempty"`
	// Transport (since 0.7.0) holds the transport settings that sessions with the peer
	// use at this node in the place of the node's, by name, each written as on the
	// command line ("window": "256MB"); absent when the peer has none.
	Transport map[string]string `json:"transport,omitempty"`
}

// Path describes one way to a peer. A proxy is shown without its user and password.
// Addrs (since 0.5.0) are further addresses of the same listener, tried in their order
// after Addr; absent when the path has one address. Mark (since 0.5.0) is the socket
// mark of the path's connections; absent when it has none.
type Path struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Addr     string   `json:"addr"`
	Addrs    []string `json:"addrs,omitempty"`
	Proxy    string   `json:"proxy,omitempty"`
	Security string   `json:"security"`
	Verify   string   `json:"verify,omitempty"`
	Pin      string   `json:"pin,omitempty"`
	Roles    []string `json:"roles"`
	Subs     int      `json:"subs"`
	Mark     int      `json:"mark,omitempty"`
	// ProxyLogin (since 0.10.0) says that the proxy has a user and a password in the
	// node, which no answer shows. It is read-only: a request that carries it changes
	// nothing by it.
	ProxyLogin bool `json:"proxy_login,omitempty"`
}

// PeerUpdate is the body of PUT /api/v1/peers/{name}. A field that is absent leaves
// that setting as it is; at least one must be there. Paths replaces the peer's paths as
// a whole (an empty list leaves the peer without a path). Allow is a list of networks
// with optional ports ("192.0.2.0/24:873, [2001:db8::/32]:5000-5010"); empty means any
// destination. UDPDial is a host:port; empty means none.
//
// Since 0.7.0: transport sets transport settings for the sessions with the peer, by
// name. A name with a value is set to it, a name with null goes back to what the node
// has, and a name that is not there stays as it is.
type PeerUpdate struct {
	Paths   *[]Path `json:"paths"`
	Allow   *string `json:"allow"`
	UDPDial *string `json:"udp_dial"`
	// left out of a request that does not set it, so that a node from before 0.7.0
	// takes a request that only names what it knows
	Transport map[string]*string `json:"transport,omitempty"`
}

// PathUpdate is the body of PUT /api/v1/peers/{name}/paths/{path} (since 0.4.0): what
// to set on that one path. A field that is absent stays as it is; at least one must be
// there. A path that the peer does not have yet is added: it needs addr, and what the
// request does not name is type tcp, security tls, the data role and 8 sub-connections.
// The node changes the one path in one step, so requests for two paths of a peer that
// arrive at the same time both take effect, which a PUT of the whole list cannot
// promise.
//
// Since 0.5.0: addrs sets the further addresses of the path (an empty list: none), and
// a request that sets addr without addrs takes them away. mark sets the socket mark (0:
// none).
type PathUpdate struct {
	Addr *string `json:"addr"`
	// left out of a request that does not set them, so that a node from before 0.5.0
	// takes a request that only names what it knows
	Addrs    *[]string `json:"addrs,omitempty"`
	Mark     *int      `json:"mark,omitempty"`
	Type     *string   `json:"type"`
	Proxy    *string   `json:"proxy"`
	Security *string   `json:"security"`
	Verify   *string   `json:"verify"`
	Pin      *string   `json:"pin"`
	Roles    *[]string `json:"roles"`
	Subs     *int      `json:"subs"`
}

// PathSet is the answer of PUT /api/v1/peers/{name}/paths/{path}: whether the path is
// new, and the peer with all its paths as stored.
type PathSet struct {
	Created bool `json:"created"`
	Peer    Peer `json:"peer"`
}

// RuleSpec is the body of POST /api/v1/rules and PUT /api/v1/rules/{name}: a fixed rule
// (listen and destination, each IP:port or IP:first-last, with ranges of the same
// length) or a transparent rule (match, a list of networks with optional ports). Since
// 0.4.0 the destination may have a host name in the place of the IP, which the peer
// looks up for every connection. Enabled is true when absent. In a PUT, an absent name
// means the name in the path.
//
// Since 0.5.0: paths names the paths the rule's connections use (an empty list: every
// path), and subs the most data sub-connections on each (0: as many as the path has).
// In a PUT, either one that is absent stays as the stored rule has it, so that a
// caller that does not know the two fields does not take them away.
type RuleSpec struct {
	Name        string `json:"name"`
	Listen      string `json:"listen,omitempty"`
	Destination string `json:"destination,omitempty"`
	Match       string `json:"match,omitempty"`
	// Socks (since 0.9.0) makes the rule a SOCKS5 rule: the address this node listens
	// on for SOCKS5 clients, "IP:port", or ":port" for the loopback address
	// 127.0.0.1. In a PUT that names none of listen, destination, match and socks,
	// the rule keeps the ones it has.
	Socks   string    `json:"socks,omitempty"`
	Peer    string    `json:"peer"`
	Enabled *bool     `json:"enabled,omitempty"`
	Paths   *[]string `json:"paths,omitempty"`
	Subs    *int      `json:"subs,omitempty"`
	// Dial (since 0.6.0) says which node opens the sub-connections of the rule's
	// connections: "auto", "self", "peer" or "both". In a POST, absent means "auto"; in
	// a PUT, absent leaves the rule as it is.
	Dial *string `json:"dial,omitempty"`
	// Transport (since 0.7.0) sets transport settings for the rule's connections, by
	// name, as in a PeerUpdate: a name with a value is set, a name with null goes back
	// to what the peer or the node has, and in a PUT a name that is not there stays as
	// the stored rule has it.
	Transport map[string]*string `json:"transport,omitempty"`
}

// Rule describes a forwarding rule and how it is doing.
type Rule struct {
	Name        string `json:"name"`
	Listen      string `json:"listen,omitempty"`
	Destination string `json:"destination,omitempty"`
	Match       string `json:"match,omitempty"`
	// Socks (since 0.9.0): the address a SOCKS5 rule listens on; absent for the other
	// kinds of rule.
	Socks     string `json:"socks,omitempty"`
	Peer      string `json:"peer"`
	Enabled   bool   `json:"enabled"`
	Listening bool   `json:"listening"`       // connections for the rule are being taken
	Error     string `json:"error,omitempty"` // why not, or what went wrong with the last connection
	Sessions  int    `json:"sessions"`        // live sessions the rule carries
	// ErrorAt is when what Error says happened (since 0.3.0); absent where Error is a
	// state that holds now and not an event.
	ErrorAt *time.Time `json:"error_at,omitempty"`
	// Paths and Subs (since 0.5.0): the paths the rule's connections use and the most
	// data sub-connections on each; absent when the rule uses everything.
	Paths []string `json:"paths,omitempty"`
	Subs  int      `json:"subs,omitempty"`
	// PeerDials (since 0.5.0): new connections of the rule are opened by the peer for
	// now, because this node's own path to the peer is down. Absent otherwise.
	PeerDials bool `json:"peer_dials,omitempty"`
	// Dial (since 0.6.0): "self", "peer" or "both" when the rule says which node
	// opens the sub-connections; absent for "auto".
	Dial string `json:"dial,omitempty"`
	// Transport (since 0.7.0): the transport settings the rule's connections use in
	// the place of the peer's and the node's; absent when the rule has none.
	Transport map[string]string `json:"transport,omitempty"`
}

// Transport is the answer of GET and PUT /api/v1/transport (since 0.7.0): every
// transport setting of the node, in a fixed order.
type Transport struct {
	Settings []TransportSetting `json:"settings"`
}

// TransportSetting is one transport setting of the node. Value is what a session that
// is created now gets, unless its peer or its rule sets the same name; it is written as
// on the command line ("256MB", "300ms", "5", "true"). Source says where the value
// comes from: "default", "command line" (given when the node was started, and not to
// be changed while it runs) or "stored" (set while a node ran). Stored is the value
// kept in the node's state, also where the command line goes before it. Shared marks a
// setting that all sessions of the node share: a peer and a rule cannot set it.
type TransportSetting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Default string `json:"default"`
	Source  string `json:"source"`
	Stored  string `json:"stored,omitempty"`
	Shared  bool   `json:"shared,omitempty"`
}

// TransportUpdate is the body of PUT /api/v1/transport: under "set", a name with a
// value is set to it and a name with null goes back to its default. At least one name
// must be there. The change acts on sessions that are created afterwards.
type TransportUpdate struct {
	Set map[string]*string `json:"set"`
}

// PeerRemoval is the answer of DELETE /api/v1/peers/{name} when the request has the
// query "rules" (since 0.4.0): "rules=keep" removes the peer and lists the rules that
// still name it, "rules=remove" removes those rules with it. Without the query the
// answer is 204 with no body, as before, and the rules stay.
type PeerRemoval struct {
	Peer string `json:"peer"`
	// Rules are the names of the rules that named the peer, in the order of the list.
	Rules []string `json:"rules"`
	// RulesRemoved: they were removed with the peer. Otherwise they are still there,
	// and a node that is paired under the same name later takes them over.
	RulesRemoved bool `json:"rules_removed"`
}

// PeerCheck is the answer of POST /api/v1/peers/{name}/check.
type PeerCheck struct {
	Peer  string      `json:"peer"`
	Paths []PathCheck `json:"paths"` // empty: this node has no path to the peer
}

// PathCheck is the outcome of the check of one path at one address. Since 0.5.0 a path
// with several addresses has one entry for each, in the order of the path.
type PathCheck struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Security string `json:"security"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// PeerTestRequest is the body of POST /api/v1/peers/{name}/test. Both fields are
// optional: Seconds is how long each run lasts (1 to 60; absent or 0: 10), Path names
// the one path to test (absent or empty: every path that carries data).
type PeerTestRequest struct {
	Seconds int    `json:"seconds"`
	Path    string `json:"path"`
}

// PeerTest is the answer of POST /api/v1/peers/{name}/test.
type PeerTest struct {
	Peer    string `json:"peer"`
	Seconds int    `json:"seconds"` // how long each run lasted at most
	// PeerDials: this node has no path to the peer, and the paths are the ones the peer
	// holds to this node.
	PeerDials bool       `json:"peer_dials"`
	Paths     []PathTest `json:"paths"`
}

// PathTest is the outcome of a path test for one path: the bytes a second that the
// peer reported as received with one data sub-connection, and with all the path has
// (0 when it has one). LittleGain says that all of them carried less than 1.5 times
// what one carried. With Error, the path could not be measured.
type PathTest struct {
	Name       string  `json:"name"`
	Subs       int     `json:"subs"`
	One        float64 `json:"one_bytes_per_second"`
	All        float64 `json:"all_bytes_per_second"`
	LittleGain bool    `json:"little_gain"`
	Error      string  `json:"error,omitempty"`
}

// ConnectUpgrade names the protocol of a connection after a connect request was
// answered with 101: the stream itself.
const ConnectUpgrade = "tcpraid-connect"

// SessionHeader is the header of the 101 answer to a connect request that holds the id
// of the session that carries the connection (since 0.11.0).
const SessionHeader = "Tcpraid-Session"

// SessionEnd is the answer of GET /api/v1/sessions/{id}/end (since 0.11.0): how a
// session ended at the node. Ended is false when the session had not ended within the
// time the request waited. Complete says that the stream ended completely in both
// directions: the other node wrote all of this end's data to the destination and
// ended that direction in order, and all of the destination's data was handed over.
// Reason is the reason as the node's log has it.
type SessionEnd struct {
	Ended    bool   `json:"ended"`
	Complete bool   `json:"complete"`
	Reason   string `json:"reason,omitempty"`
}

// ConnectRequest asks for a connection to host:port as seen from the peer. Host is an
// IP address or a host name, which the peer looks up.
type ConnectRequest struct {
	Peer string `json:"peer"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// NodeUpdate is the body of PUT /api/v1/node.
type NodeUpdate struct {
	Name string `json:"name"`
}
