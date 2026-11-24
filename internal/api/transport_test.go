package api

import (
	"fmt"
	"strings"
	"testing"
)

func settingOf(t *testing.T, tr Transport, name string) TransportSetting {
	t.Helper()
	for _, s := range tr.Settings {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("the answer has no setting called %q", name)
	return TransportSetting{}
}

// The node's transport settings are read and set by name, one request for several. A
// name with null goes back to its default. What is no setting, or no value of one,
// is the caller's mistake and changes nothing.
func TestTransportRoutes(t *testing.T) {
	a := newPeerNode(t, "alpha")
	tr := decode[Transport](t, a.send("GET", PathTransport, ""), 200)
	if len(tr.Settings) < 10 {
		t.Fatalf("%d settings listed", len(tr.Settings))
	}
	if s := settingOf(t, tr, "window"); s.Value != "128MB" || s.Default != "128MB" || s.Source != "default" || s.Stored != "" || s.Shared {
		t.Errorf("window: %+v", s)
	}
	if s := settingOf(t, tr, "budget"); !s.Shared || s.Value != "256MB" {
		t.Errorf("budget: %+v", s)
	}
	raw := a.send("GET", PathTransport, "").Body.String()
	for _, absent := range []string{"chunk", "maxrss", "pair_secret", "token"} {
		if strings.Contains(raw, absent) {
			t.Errorf("the answer names %q: %s", absent, raw)
		}
	}

	tr = decode[Transport](t, a.send("PUT", PathTransport, `{"set":{"window":"67108864","target":"0.5s","share":"false"}}`), 200)
	if s := settingOf(t, tr, "window"); s.Value != "64MB" || s.Source != "stored" || s.Stored != "64MB" {
		t.Errorf("window after it was set: %+v", s)
	}
	if s := settingOf(t, tr, "target"); s.Value != "500ms" || s.Source != "stored" {
		t.Errorf("target after it was set: %+v", s)
	}
	if s := settingOf(t, tr, "share"); s.Value != "false" {
		t.Errorf("share after it was set: %+v", s)
	}
	tr = decode[Transport](t, a.send("PUT", PathTransport, `{"set":{"window":null,"share":null}}`), 200)
	if s := settingOf(t, tr, "window"); s.Value != "128MB" || s.Source != "default" || s.Stored != "" {
		t.Errorf("window after it went back: %+v", s)
	}
	if s := settingOf(t, tr, "target"); s.Value != "500ms" {
		t.Errorf("a setting the request did not name changed: %+v", s)
	}

	wantError(t, "nothing to set", a.send("PUT", PathTransport, `{}`), 400)
	wantError(t, "nothing to set", a.send("PUT", PathTransport, `{"set":{}}`), 400)
	wantError(t, "not a setting", a.send("PUT", PathTransport, `{"set":{"chunk":"128KB"}}`), 400)
	wantError(t, "out of range", a.send("PUT", PathTransport, `{"set":{"dead":"1s"}}`), 400)
	wantError(t, "one bad among good", a.send("PUT", PathTransport, `{"set":{"window":"32MB","fbsubs":"0"}}`), 400)
	wantError(t, "a number where text belongs", a.send("PUT", PathTransport, `{"set":{"fbsubs":4}}`), 400)
	wantError(t, "unknown field", a.send("PUT", PathTransport, `{"window":"32MB"}`), 400)
	wantError(t, "no such method", a.send("DELETE", PathTransport, ""), 405)
	if s := settingOf(t, decode[Transport](t, a.send("GET", PathTransport, ""), 200), "window"); s.Value != "128MB" {
		t.Errorf("a refused request changed the window: %+v", s)
	}
}

// A peer and a rule carry transport settings of their own: set by name, shown in their
// records, left alone by a request that does not name them, and refused where the
// setting is one that all sessions of the node share.
func TestTransportOfPeersAndRules(t *testing.T) {
	a, b := newPeerNode(t, "alpha"), newPeerNode(t, "beta")
	pairOver(t, a, b)
	if raw := b.send("GET", PathPeers+"/alpha", "").Body.String(); strings.Contains(raw, "transport") {
		t.Errorf("a peer without settings names them: %s", raw)
	}
	p := decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"transport":{"window":"32MB","grace":"300s"}}`), 200)
	if p.Transport["window"] != "32MB" || p.Transport["grace"] != "5m0s" || len(p.Transport) != 2 {
		t.Errorf("the peer's settings: %v", p.Transport)
	}
	// a request for something else leaves them, as a caller from before 0.7.0 sends it
	p = decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"allow":"192.0.2.0/24"}`), 200)
	if len(p.Transport) != 2 || p.Allow != "192.0.2.0/24" {
		t.Errorf("after a request that names no setting: %+v", p)
	}
	p = decode[Peer](t, b.send("PUT", PathPeers+"/alpha", `{"transport":{"window":null,"hol":"2s"}}`), 200)
	if p.Transport["hol"] != "2s" || p.Transport["grace"] != "5m0s" || len(p.Transport) != 2 {
		t.Errorf("after one was taken off and one added: %v", p.Transport)
	}
	wantError(t, "a setting all sessions share", b.send("PUT", PathPeers+"/alpha", `{"transport":{"budget":"1GB"}}`), 400)
	wantError(t, "out of range", b.send("PUT", PathPeers+"/alpha", `{"transport":{"target":"1h"}}`), 400)
	wantError(t, "nothing", b.send("PUT", PathPeers+"/alpha", `{"transport":{}}`), 400)
	wantError(t, "no such peer", b.send("PUT", PathPeers+"/nobody", `{"transport":{"window":"32MB"}}`), 404)

	in := freePort(t)
	spec := fmt.Sprintf(`{"name":"zfs","listen":%q,"destination":"192.0.2.1:873","peer":"alpha"`, in)
	made := decode[Rule](t, b.send("POST", PathRules, spec+`,"transport":{"window":"16777216","dead":"30s"}}`), 201)
	if made.Transport["window"] != "16MB" || made.Transport["dead"] != "30s" || len(made.Transport) != 2 || !made.Listening {
		t.Errorf("the rule as made: %+v", made)
	}
	// a PUT without the field keeps the settings, and the rule goes on listening
	kept := decode[Rule](t, b.send("PUT", PathRules+"/zfs", spec+`}`), 200)
	if len(kept.Transport) != 2 || !kept.Listening {
		t.Errorf("after a PUT that names no setting: %+v", kept)
	}
	changed := decode[Rule](t, b.send("PUT", PathRules+"/zfs", spec+`,"transport":{"window":null,"fbsubs":"5"}}`), 200)
	if changed.Transport["fbsubs"] != "5" || changed.Transport["dead"] != "30s" || len(changed.Transport) != 2 {
		t.Errorf("after one was taken off and one added: %v", changed.Transport)
	}
	wantError(t, "a setting all sessions share", b.send("PUT", PathRules+"/zfs", spec+`,"transport":{"timeline":"1s"}}`), 400)
	wantError(t, "not a setting", b.send("POST", PathRules, `{"name":"x","listen":"127.0.0.1:1","destination":"192.0.2.1:1","peer":"alpha","transport":{"chunk":"1MB"}}`), 400)
	bare := decode[Rule](t, b.send("PUT", PathRules+"/zfs", spec+`,"transport":{"fbsubs":null,"dead":null}}`), 200)
	if raw := b.send("GET", PathRules, "").Body.String(); len(bare.Transport) != 0 || strings.Contains(raw, "transport") {
		t.Errorf("a rule without settings names them: %s", raw)
	}
}
