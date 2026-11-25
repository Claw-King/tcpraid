package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Claw-King/tcpraid/internal/engine"
)

// A label value is written as the text format asks: the backslash, the double quote
// and the line feed are escaped, and nothing else is changed. The samples of one name
// follow their one TYPE line.
func TestMetricsTextEscapesLabelValues(t *testing.T) {
	text := string(MetricsText([]MetricsSample{
		{Name: "tcpraid_peer_online", Type: "gauge", Label: "peer", Key: "plain", Value: 1},
		{Name: "tcpraid_peer_online", Type: "gauge", Label: "peer", Key: "a\"b\\c\nd", Value: 0},
		{Name: "tcpraid_peer_online", Type: "gauge", Label: "peer", Key: "nas \u00e9\u4e2d", Value: 1},
		{Name: "tcpraid_sessions_total", Type: "counter", Value: 7},
	}))
	want := "# TYPE tcpraid_peer_online gauge\n" +
		"tcpraid_peer_online{peer=\"plain\"} 1\n" +
		"tcpraid_peer_online{peer=\"a\\\"b\\\\c\\nd\"} 0\n" +
		"tcpraid_peer_online{peer=\"nas \u00e9\u4e2d\"} 1\n" +
		"# TYPE tcpraid_sessions_total counter\n" +
		"tcpraid_sessions_total 7\n"
	if text != want {
		t.Errorf("got:\n%s\nwant:\n%s", text, want)
	}
}

// The metrics listener answers GET for its two paths, without a token, and at most
// MaxMetricsRequests requests at one time: one more is told to come back.
func TestMetricsServerIsBounded(t *testing.T) {
	m := NewMetricsServer(MetricsSource{Engine: engine.New(engine.DefaultConfig())})
	get := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	if w := get("GET", "/metrics"); w.Code != 200 || !strings.Contains(w.Body.String(), "tcpraid_sessions_total 0\n") {
		t.Fatalf("GET /metrics: %d", w.Code)
	}
	if w := get("GET", "/metrics.json"); w.Code != 200 || !strings.Contains(w.Body.String(), `{"name":"tcpraid_sessions_total","type":"counter","value":0}`) {
		t.Fatalf("GET /metrics.json: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{{"POST", "/metrics", 405}, {"PUT", "/metrics.json", 405}, {"GET", "/api/v1/status", 404}, {"GET", "/healthz", 404}, {"DELETE", "/", 404}} {
		if w := get(c.method, c.path); w.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
	// every place taken: the next request is refused at once, and served again later
	for i := 0; i < MaxMetricsRequests; i++ {
		m.slots <- struct{}{}
	}
	if w := get("GET", "/metrics"); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("a request beyond the %d that are answered at one time: %d", MaxMetricsRequests, w.Code)
	}
	for i := 0; i < MaxMetricsRequests; i++ {
		<-m.slots
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := get("GET", "/metrics"); w.Code != 200 && w.Code != http.StatusServiceUnavailable {
				t.Errorf("a request among many: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(m.slots) != 0 {
		t.Errorf("%d places are still taken after every request was answered", len(m.slots))
	}
}
