package api

// The node's figures for a monitoring system.
//
// They are served in two places. GET /metrics of the API needs the token, like every
// path of the API. A node started with -metrics also serves them on an address of their
// own, where no token is asked for (MetricsServer):
//
//	GET /metrics        the Prometheus text format
//	GET /metrics.json   the same samples as JSON
//
// The figures are numbers: the engine's counters, for each peer whether a control
// connection with it is up, for each rule whether it takes connections and how many
// sessions it carries, the memory of the process, and how the hook fares. The names of
// the peers and of the rules are in them, as label values. No address is, no
// destination, no key or token, and no text of a failure.
//
// The text format is written as its documentation describes it
// (https://prometheus.io/docs/instrumenting/exposition_formats/): one sample to a line,
// "name value" or "name{label="value"} value", every line ended by a line feed; a
// "# TYPE name counter|gauge" line before the first sample of a name; all samples of a
// name in one group; and in a label value the backslash, the double quote and the line
// feed written as \\, \" and \n.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	runtimemetrics "runtime/metrics"
	"strconv"
	"strings"
	"time"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// MetricsSample is one figure: a name, at most one label, and a whole number.
type MetricsSample struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // "counter" or "gauge"
	Label string `json:"label,omitempty"`
	Key   string `json:"key,omitempty"` // the value of the label
	Value int64  `json:"value"`
}

// MetricsSource is where the figures come from. Engine must be set; without Node or
// Rules the figures of the peers and the rules are left out. Extra, if not nil, adds
// figures of its own (the hook's).
type MetricsSource struct {
	Engine *engine.Engine
	Node   Node
	Rules  Rules
	Extra  func() []MetricsSample
}

// Samples returns the figures as they are now, the samples of one name next to each
// other.
func (m MetricsSource) Samples() []MetricsSample {
	var out []MetricsSample
	for _, c := range m.Engine.Counters() {
		kind := "counter"
		if c.Gauge {
			kind = "gauge"
		}
		out = append(out, MetricsSample{Name: "tcpraid_" + c.Name, Type: kind, Label: c.Label, Key: c.Key, Value: c.Value})
	}
	// what the Go runtime has from the system, on every system; rss_mb above is there
	// only where the system says what the process holds
	mem := []runtimemetrics.Sample{{Name: "/memory/classes/total:bytes"}}
	runtimemetrics.Read(mem)
	if mem[0].Value.Kind() == runtimemetrics.KindUint64 {
		out = append(out, MetricsSample{Name: "tcpraid_runtime_memory_mb", Type: "gauge", Value: int64(mem[0].Value.Uint64() >> 20)})
	}
	if m.Node != nil && m.Rules != nil {
		for _, p := range m.Node.Peers() {
			v := int64(0)
			if m.Rules.PeerStatus(p.Name).Online {
				v = 1
			}
			out = append(out, MetricsSample{Name: "tcpraid_peer_online", Type: "gauge", Label: "peer", Key: p.Name, Value: v})
		}
		rules := m.Rules.Rules()
		for _, r := range rules {
			v := int64(0)
			if r.Listening {
				v = 1
			}
			out = append(out, MetricsSample{Name: "tcpraid_rule_listening", Type: "gauge", Label: "rule", Key: r.Rule.Name, Value: v})
		}
		for _, r := range rules {
			out = append(out, MetricsSample{Name: "tcpraid_rule_sessions", Type: "gauge", Label: "rule", Key: r.Rule.Name, Value: int64(r.Sessions)})
		}
	}
	if m.Extra != nil {
		out = append(out, m.Extra()...)
	}
	return out
}

// escapeLabel writes a label value as the text format asks: the backslash, the double
// quote and the line feed are escaped, and every other character stays as it is.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// MetricsText writes the samples in the Prometheus text format.
func MetricsText(samples []MetricsSample) []byte {
	var b bytes.Buffer
	typed := map[string]bool{}
	for _, s := range samples {
		if !typed[s.Name] {
			typed[s.Name] = true
			fmt.Fprintf(&b, "# TYPE %s %s\n", s.Name, s.Type)
		}
		if s.Label != "" {
			fmt.Fprintf(&b, "%s{%s=\"%s\"} %s\n", s.Name, s.Label, escapeLabel(s.Key), strconv.FormatInt(s.Value, 10))
		} else {
			fmt.Fprintf(&b, "%s %s\n", s.Name, strconv.FormatInt(s.Value, 10))
		}
	}
	return b.Bytes()
}

// metricsContentType is what the documentation of the text format gives: text/plain
// with the parameter version=0.0.4.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// MaxMetricsRequests is how many requests the metrics listener answers at one time.
// One more is answered 503 at once.
const MaxMetricsRequests = 4

// MetricsServer serves the figures on an address of their own, without a token. It
// answers GET for its two paths and nothing else, changes nothing in the node, and
// answers at most MaxMetricsRequests requests at one time.
type MetricsServer struct {
	Source MetricsSource
	slots  chan struct{}
}

// NewMetricsServer returns a MetricsServer for the figures of src.
func NewMetricsServer(src MetricsSource) *MetricsServer {
	return &MetricsServer{Source: src, slots: make(chan struct{}, MaxMetricsRequests)}
}

func (m *MetricsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "too many requests for the metrics at one time")
		return
	}
	if r.URL.Path != PathMetrics && r.URL.Path != PathMetrics+".json" {
		writeError(w, http.StatusNotFound, "no such path")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "this path answers GET")
		return
	}
	samples := m.Source.Samples()
	if r.URL.Path == PathMetrics {
		w.Header().Set("Content-Type", metricsContentType)
		w.Write(MetricsText(samples))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Metrics []MetricsSample `json:"metrics"`
	}{samples})
}

// Serve answers requests on ln until ctx is done. A request has 5 seconds to arrive
// and its answer 10 seconds to be taken.
func (m *MetricsServer) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler:                      m,
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  5 * time.Second,
		WriteTimeout:                 10 * time.Second,
		IdleTimeout:                  30 * time.Second,
		MaxHeaderBytes:               8 << 10,
		ErrorLog:                     log.New(serverLog{}, "", 0),
		DisableGeneralOptionsHandler: true,
	}
	shut := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(shut)
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if hs.Shutdown(c) != nil {
			hs.Close()
		}
	})
	err := hs.Serve(ln)
	if stop() {
		return err // ctx is not done: the listener failed
	}
	<-shut
	return nil
}
