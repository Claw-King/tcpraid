package main

// What the node tells its hook (-hook) and what it adds to its figures about it.

import (
	"encoding/hex"

	"github.com/Claw-King/tcpraid/internal/api"
	"github.com/Claw-King/tcpraid/internal/engine"
	"github.com/Claw-King/tcpraid/internal/forward"
	"github.com/Claw-King/tcpraid/internal/hook"
)

// sessionEvent is the hook's event for a session that was created or has ended. A
// session of a path test carries no application connection and is not told: the
// caller leaves those out (Owner.Test).
func sessionEvent(ev engine.SessionEvent) hook.Event {
	out := hook.Event{Event: hook.SessionStart, Peer: ev.Owner.Peer, Rule: ev.Owner.Rule, Session: hex.EncodeToString(ev.ID[:]), Role: "server", Destination: ev.Destination}
	if ev.Client {
		out.Role = "client"
	}
	if ev.Ended {
		tx, rx, clean := ev.TxBytes, ev.RxBytes, ev.Clean
		out.Event, out.TxBytes, out.RxBytes, out.Clean, out.Reason = hook.SessionEnd, &tx, &rx, &clean, ev.Why
	}
	return out
}

// noticeEvent is the hook's event for what the rules runtime reports.
func noticeEvent(n forward.Notice) hook.Event {
	switch n.Kind {
	case forward.NoticeOnline:
		return hook.Event{Event: hook.PeerOnline, Peer: n.Peer}
	case forward.NoticeOffline:
		return hook.Event{Event: hook.PeerOffline, Peer: n.Peer}
	}
	return hook.Event{Event: hook.RuleFailure, Peer: n.Peer, Rule: n.Rule, Reason: n.Reason}
}

// hookSamples are the figures about the hook: none when the node has no hook.
func hookSamples(h *hook.Runner) []api.MetricsSample {
	if h == nil {
		return nil
	}
	runs, dropped, failed, timeouts := h.Counts()
	return []api.MetricsSample{
		{Name: "tcpraid_hook_running", Type: "gauge", Value: int64(h.Running())},
		{Name: "tcpraid_hook_runs", Type: "counter", Value: runs},
		{Name: "tcpraid_hook_events_dropped", Type: "counter", Value: dropped},
		{Name: "tcpraid_hook_runs_failed", Type: "counter", Value: failed},
		{Name: "tcpraid_hook_runs_timed_out", Type: "counter", Value: timeouts},
	}
}
