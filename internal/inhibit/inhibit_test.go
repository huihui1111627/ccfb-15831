package inhibit

import (
	"testing"
	"time"

	"kilnalert/internal/model"
)

func l(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestEqualLabels(t *testing.T) {
	rules, err := Compile([]model.InhibitRule{{
		ID:     "power",
		Source: model.MatchAll{{Name: "alertname", Op: "=", Value: "PowerLoss"}},
		Target: model.MatchAll{{Name: "alertname", Op: "=", Value: "OverTemp"}},
		Equal:  []string{"zone"},
	}}, "n")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	src := model.Alert{ID: "s1", Labels: l("alertname", "PowerLoss", "zone", "A"), Status: model.StatusFiring}
	tgtSameZone := model.Alert{ID: "t1", Labels: l("alertname", "OverTemp", "zone", "A"), Status: model.StatusFiring}
	tgtOtherZone := model.Alert{ID: "t2", Labels: l("alertname", "OverTemp", "zone", "B"), Status: model.StatusFiring}
	if d := Evaluate(rules, tgtSameZone, []model.Alert{src}); !d.Inhibited || d.RuleID != "power" {
		t.Fatal("same-zone target should be inhibited")
	}
	if d := Evaluate(rules, tgtOtherZone, []model.Alert{src}); d.Inhibited {
		t.Fatal("other-zone target must not be inhibited")
	}
	// Once the source resolves it no longer appears in the firing set.
	if d := Evaluate(rules, tgtSameZone, nil); d.Inhibited {
		t.Fatal("target must recover when the source resolves")
	}
	_ = now
}

func TestSelfInhibitionIgnored(t *testing.T) {
	rules, err := Compile([]model.InhibitRule{{
		ID:     "self",
		Source: model.MatchAll{{Name: "kind", Op: "=", Value: "x"}},
		Target: model.MatchAll{{Name: "kind", Op: "=", Value: "x"}},
		Equal:  []string{"kiln"},
	}}, "n")
	if err != nil {
		t.Fatal(err)
	}
	a := model.Alert{ID: "a", Labels: l("kind", "x", "kiln", "1"), Status: model.StatusFiring}
	if d := Evaluate(rules, a, []model.Alert{a}); d.Inhibited {
		t.Fatal("an alert must not inhibit itself")
	}
}

func TestMissingLabelCountsAsEmpty(t *testing.T) {
	rules, _ := Compile([]model.InhibitRule{{
		ID:     "byline",
		Source: model.MatchAll{{Name: "alertname", Op: "=", Value: "PowerLoss"}},
		Target: model.MatchAll{{Name: "alertname", Op: "=", Value: "OverTemp"}},
		Equal:  []string{"line"},
	}}, "n")
	src := model.Alert{ID: "s", Labels: l("alertname", "PowerLoss"), Status: model.StatusFiring}
	tgt := model.Alert{ID: "t", Labels: l("alertname", "OverTemp"), Status: model.StatusFiring}
	if d := Evaluate(rules, tgt, []model.Alert{src}); !d.Inhibited {
		t.Fatal("two alerts both missing the equal label agree on it")
	}
}
