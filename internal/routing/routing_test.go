package routing

import (
	"testing"
	"time"

	"kilnalert/internal/model"
	"kilnalert/internal/timewin"
)

func eq(name, value string) model.Matcher { return model.Matcher{Name: name, Op: "=", Value: value} }

func nightWindow(t *testing.T) model.TimeWindow {
	t.Helper()
	w, err := timewin.NewWindow("22:00", "06:00", nil, "Asia/Shanghai", nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestContinueSiblings(t *testing.T) {
	root := &model.RouteNode{ID: "root", Name: "根", Children: []*model.RouteNode{
		{ID: "kiln12", Name: "12号窑", Match: model.MatchAll{eq("kiln", "12")}},
		{ID: "power", Name: "断电", Match: model.MatchAll{eq("alertname", "PowerLoss")}},
	}}
	tree, err := Compile(root)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"kiln": "12", "alertname": "PowerLoss"}
	got := tree.Route(labels, time.Now())
	if len(got) != 1 || got[0].Node.Spec.ID != "kiln12" {
		t.Fatalf("without ContinueSiblings expected only kiln12, got %v", names(got))
	}
	root.Children[0].ContinueSiblings = true
	tree, _ = Compile(root)
	got = tree.Route(labels, time.Now())
	if len(got) != 2 || got[0].Node.Spec.ID != "kiln12" || got[1].Node.Spec.ID != "power" {
		t.Fatalf("with ContinueSiblings expected both, got %v", names(got))
	}
}

func names(ms []Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Node.Spec.ID
	}
	return out
}

func TestNestedStopsSubtreeButContinuesSiblings(t *testing.T) {
	root := &model.RouteNode{ID: "root", Name: "根", Children: []*model.RouteNode{
		{ID: "a", Name: "a", Match: model.MatchAll{eq("alertname", "A")}, Children: []*model.RouteNode{
			{ID: "a1", Name: "a1", Match: model.MatchAll{eq("kiln", "1")}},
		}},
		{ID: "b", Name: "b", Match: model.MatchAll{eq("kiln", "1")}},
	}}
	tree, err := Compile(root)
	if err != nil {
		t.Fatal(err)
	}
	got := tree.Route(map[string]string{"alertname": "A", "kiln": "1"}, time.Now())
	// a matches, deepest match a1 wins, and without ContinueSiblings b is not
	// tried even though it would match.
	if len(got) != 1 || got[0].Node.Spec.ID != "a1" {
		t.Fatalf("got %v", names(got))
	}
}

func TestTimeGatedNodeSkipped(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	root := &model.RouteNode{ID: "root", Name: "根", Children: []*model.RouteNode{
		{ID: "night", Name: "夜班", Match: model.MatchAll{eq("kiln", "1")}, TimeWindows: []model.TimeWindow{nightWindow(t)}},
		{ID: "day", Name: "白班", Match: model.MatchAll{eq("kiln", "1")}},
	}}
	tree, err := Compile(root)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 3, 2, 10, 0, 0, 0, sh)
	night := time.Date(2026, 3, 2, 23, 0, 0, 0, sh)
	if got := tree.Route(map[string]string{"kiln": "1"}, day); len(got) != 1 || got[0].Node.Spec.ID != "day" {
		t.Fatalf("day: got %v", names(got))
	}
	if got := tree.Route(map[string]string{"kiln": "1"}, night); len(got) != 1 || got[0].Node.Spec.ID != "night" {
		t.Fatalf("night: got %v", names(got))
	}
}

func TestMatchers(t *testing.T) {
	labels := map[string]string{"severity": "critical", "kiln": "12"}
	cases := []struct {
		m    model.Matcher
		want bool
	}{
		{model.Matcher{Name: "severity", Op: "=", Value: "critical"}, true},
		{model.Matcher{Name: "severity", Op: "!=", Value: "warning"}, true},
		{model.Matcher{Name: "missing", Op: "!=", Value: "x"}, true},
		{model.Matcher{Name: "missing", Op: "=", Value: "x"}, false},
		{model.Matcher{Name: "kiln", Op: "=~", Value: "1[0-9]"}, true},
		{model.Matcher{Name: "kiln", Op: "=~", Value: "1"}, false}, // full anchor
		{model.Matcher{Name: "kiln", Op: "!~", Value: "1[0-9]"}, false},
	}
	for _, c := range cases {
		if got := Matches(model.MatchAll{c.m}, labels); got != c.want {
			t.Errorf("%+v: got %v want %v", c.m, got, c.want)
		}
	}
}

func TestValidation(t *testing.T) {
	bad := &model.RouteNode{ID: "root", Name: "根", Children: []*model.RouteNode{
		{ID: "x", Name: "x", Match: model.MatchAll{{Name: "a", Op: "=~", Value: "("}}},
	}}
	if err := Validate(bad); err == nil {
		t.Fatal("expected error for bad regex")
	}
	dup := &model.RouteNode{ID: "root", Name: "根", Children: []*model.RouteNode{
		{ID: "x", Name: "x"}, {ID: "x", Name: "y"},
	}}
	if err := Validate(dup); err == nil {
		t.Fatal("expected error for duplicate id")
	}
}
