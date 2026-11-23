package engine

import (
	"testing"
	"time"

	"kilnalert/internal/model"
	"kilnalert/internal/routing"
)

func testTree(t *testing.T) *routing.Tree {
	t.Helper()
	spec := &model.RouteNode{ID: "root", Name: "根", GroupBy: []string{"alertname"}, Channel: "看板", Children: []*model.RouteNode{
		{ID: "kilns", Name: "窑炉", Parent: "root", Match: model.MatchAll{{Name: "kiln", Op: "=~", Value: "[0-9]+"}}, GroupBy: []string{"kiln"}, Channel: "短信",
			Children: []*model.RouteNode{
				{ID: "temp", Name: "高温", Parent: "kilns", Match: model.MatchAll{{Name: "alertname", Op: "=", Value: "OverTemp"}}, GroupBy: []string{"alertname"}, Channel: "短信"},
			}},
	}}
	tree, err := routing.Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func labels(kiln, kind string) map[string]string {
	return map[string]string{"kiln": kiln, "alertname": kind, "severity": "warning"}
}

func newTestEngine(t *testing.T) (*Engine, time.Time) {
	t.Helper()
	t0 := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	e := New(Config{Step: time.Minute, GroupWait: 2 * time.Minute, GroupInterval: 5 * time.Minute, StaleAfter: 10 * time.Minute, Replicas: []string{"r0"}},
		testTree(t), t0, t0.Add(time.Hour))
	return e, t0
}

// tickThrough advances to `to`, re-sending every firing alert each tick, as a
// live source would. This keeps tests focused on routing and batching rather
// than on stale auto-resolution.
func tickThrough(e *Engine, from, to time.Time) {
	for t := from; !t.After(to); t = t.Add(e.cfg.Step) {
		var live []map[string]string
		for _, w := range e.Alerts() {
			if w.Status == model.StatusFiring {
				live = append(live, w.Labels)
			}
		}
		for _, l := range live {
			e.Ingest(model.AlertEvent{Labels: l, Fire: true, At: t})
		}
		e.Tick(t)
	}
}

func TestGroupingBatchesIntoOneNotification(t *testing.T) {
	e, t0 := newTestEngine(t)
	for i, kiln := range []string{"1", "2", "3"} {
		e.Ingest(model.AlertEvent{Labels: labels(kiln, "OverTemp"), Fire: true, At: t0.Add(time.Duration(i) * 10 * time.Second)})
	}
	tickThrough(e, t0, t0.Add(10*time.Minute))
	if got := len(e.Notifications()); got != 1 {
		t.Fatalf("expected one batched notification, got %d", got)
	}
	n := e.Notifications()[0]
	if len(n.Delta.Added) != 3 || len(n.Delta.Resolved) != 0 {
		t.Fatalf("expected 3 additions, got +%d -%d", len(n.Delta.Added), len(n.Delta.Resolved))
	}
	if n.NodeID != "temp" {
		t.Fatalf("deepest match wins: expected temp node, got %s", n.NodeID)
	}
}

func TestDeltaCarriesOnlyChanges(t *testing.T) {
	e, t0 := newTestEngine(t)
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	e.Ingest(model.AlertEvent{Labels: labels("2", "OverTemp"), Fire: true, At: t0})
	tickThrough(e, t0, t0.Add(10*time.Minute))
	if len(e.Notifications()) != 1 {
		t.Fatalf("expected 1 notification")
	}
	// A third joins later: the next notification mentions only it.
	e.Ingest(model.AlertEvent{Labels: labels("3", "OverTemp"), Fire: true, At: t0.Add(12 * time.Minute)})
	tickThrough(e, t0.Add(12*time.Minute), t0.Add(25*time.Minute))
	if len(e.Notifications()) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(e.Notifications()))
	}
	n := e.Notifications()[1]
	if len(n.Delta.Added) != 1 || n.Delta.Added[0].Labels["kiln"] != "3" {
		t.Fatalf("second notification should add only kiln 3, got %+v", n.Delta.Added)
	}
}

func TestResolutionNotification(t *testing.T) {
	e, t0 := newTestEngine(t)
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	tickThrough(e, t0, t0.Add(6*time.Minute))
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: false, At: t0.Add(7 * time.Minute)})
	tickThrough(e, t0.Add(7*time.Minute), t0.Add(20*time.Minute))
	ns := e.Notifications()
	if len(ns) != 2 {
		t.Fatalf("expected fire then resolve, got %d", len(ns))
	}
	if len(ns[1].Delta.Resolved) != 1 || len(ns[1].Delta.Added) != 0 {
		t.Fatalf("second notification should resolve one alert: %+v", ns[1].Delta)
	}
}

func TestStaleAutoResolve(t *testing.T) {
	e, t0 := newTestEngine(t)
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	// Tick without re-sending: the source has gone quiet.
	for t := t0; !t.After(t0.Add(30 * time.Minute)); t = t.Add(e.cfg.Step) {
		e.Tick(t)
	}
	w := e.Alerts()[model.Fingerprint(labels("1", "OverTemp"))]
	if w == nil || w.Status != model.StatusResolved {
		t.Fatalf("expected auto-resolve after stale timeout")
	}
	if w.End.Sub(t0) != e.cfg.StaleAfter {
		t.Fatalf("auto-resolve time %v, want %v", w.End.Sub(t0), e.cfg.StaleAfter)
	}
	resolved := 0
	for _, n := range e.Notifications() {
		resolved += len(n.Delta.Resolved)
	}
	if resolved != 1 {
		t.Fatalf("expected one resolution notification, got %d", resolved)
	}
}

func TestSilenceSuppressesAndExpires(t *testing.T) {
	e, t0 := newTestEngine(t)
	e.AddSilence(&model.Silence{
		ID: "s1", Match: model.MatchAll{{Name: "alertname", Op: "=", Value: "OverTemp"}},
		Starts: t0, Ends: t0.Add(20 * time.Minute),
	})
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	tickThrough(e, t0, t0.Add(15*time.Minute))
	if len(e.Notifications()) != 0 {
		t.Fatalf("silenced alert must not notify, got %d", len(e.Notifications()))
	}
	// After the silence expires, the still-firing alert is notified.
	tickThrough(e, t0.Add(16*time.Minute), t0.Add(30*time.Minute))
	ns := e.Notifications()
	if len(ns) != 1 || len(ns[0].Delta.Added) != 1 {
		t.Fatalf("expected one notification after expiry, got %+v", ns)
	}
}

func TestSilenceCancelRestores(t *testing.T) {
	e, t0 := newTestEngine(t)
	e.AddSilence(&model.Silence{ID: "s1", Match: model.MatchAll{{Name: "kiln", Op: "=", Value: "1"}}, Starts: t0, Ends: t0.Add(time.Hour)})
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	tickThrough(e, t0, t0.Add(10*time.Minute))
	if len(e.Notifications()) != 0 {
		t.Fatalf("expected silence to hold the alert")
	}
	if !e.CancelSilence("s1", t0.Add(11*time.Minute)) {
		t.Fatal("cancel failed")
	}
	tickThrough(e, t0.Add(12*time.Minute), t0.Add(25*time.Minute))
	if len(e.Notifications()) != 1 {
		t.Fatalf("expected notification after cancel, got %d", len(e.Notifications()))
	}
}

func TestInhibitionMutesTarget(t *testing.T) {
	spec := &model.RouteNode{ID: "root", Name: "根", GroupBy: []string{"alertname"}, Channel: "看板", Children: []*model.RouteNode{
		{ID: "kilns", Name: "窑炉", Parent: "root", Match: model.MatchAll{{Name: "kiln", Op: "=~", Value: "[0-9]+"}}, GroupBy: []string{"kiln"}, Channel: "短信",
			InhibitRules: []model.InhibitRule{{
				ID:     "power-mutes-temp",
				Source: model.MatchAll{{Name: "alertname", Op: "=", Value: "PowerLoss"}},
				Target: model.MatchAll{{Name: "alertname", Op: "=", Value: "OverTemp"}},
				Equal:  []string{"kiln"},
			}},
			Children: []*model.RouteNode{
				{ID: "temp", Name: "高温", Parent: "kilns", Match: model.MatchAll{{Name: "alertname", Op: "=", Value: "OverTemp"}}, GroupBy: []string{"alertname"}, Channel: "短信"},
				{ID: "power", Name: "断电", Parent: "kilns", Match: model.MatchAll{{Name: "alertname", Op: "=", Value: "PowerLoss"}}, GroupBy: []string{"kiln"}, Channel: "电话"},
			}},
	}}
	e, t0 := newTestEngineWithRules(t, spec)
	e.Ingest(model.AlertEvent{Labels: labels("1", "PowerLoss"), Fire: true, At: t0})
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	tickThrough(e, t0, t0.Add(10*time.Minute))
	// Only the power loss notifies; the temperature alert is inhibited.
	for _, n := range e.Notifications() {
		for _, a := range n.Delta.Added {
			if a.Labels["alertname"] == "OverTemp" {
				t.Fatal("inhibited alert must not be notified")
			}
		}
	}
	// Once the power alert resolves, the temperature alert is released.
	e.Ingest(model.AlertEvent{Labels: labels("1", "PowerLoss"), Fire: false, At: t0.Add(11 * time.Minute)})
	tickThrough(e, t0.Add(11*time.Minute), t0.Add(30*time.Minute))
	found := false
	for _, n := range e.Notifications() {
		for _, a := range n.Delta.Added {
			if a.Labels["alertname"] == "OverTemp" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("target alert should notify once the source resolves")
	}
}

func TestReFireIsNewEpisode(t *testing.T) {
	e, t0 := newTestEngine(t)
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0})
	tickThrough(e, t0, t0.Add(8*time.Minute))
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: false, At: t0.Add(9 * time.Minute)})
	tickThrough(e, t0.Add(9*time.Minute), t0.Add(10*time.Minute))
	e.Ingest(model.AlertEvent{Labels: labels("1", "OverTemp"), Fire: true, At: t0.Add(11 * time.Minute)})
	tickThrough(e, t0.Add(11*time.Minute), t0.Add(30*time.Minute))
	var added, resolved int
	for _, n := range e.Notifications() {
		added += len(n.Delta.Added)
		resolved += len(n.Delta.Resolved)
	}
	if added != 2 || resolved != 1 {
		t.Fatalf("expected two additions and one resolution over two episodes, got +%d -%d", added, resolved)
	}
}

func newTestEngineWithRules(t *testing.T, spec *model.RouteNode) (*Engine, time.Time) {
	t.Helper()
	t0 := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	tree, err := routing.Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Step: time.Minute, GroupWait: 2 * time.Minute, GroupInterval: 5 * time.Minute, StaleAfter: 10 * time.Minute}, tree, t0, t0.Add(time.Hour)), t0
}
