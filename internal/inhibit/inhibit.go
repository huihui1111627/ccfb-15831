// Package inhibit evaluates inhibition rules: while a source alert fires,
// targets matching the rule that share the listed labels are muted.
package inhibit

import (
	"fmt"

	"kilnalert/internal/model"
	"kilnalert/internal/routing"
)

// CompiledRule is an inhibition rule with its matchers compiled.
type CompiledRule struct {
	Spec   model.InhibitRule
	Source func(map[string]string) bool
	Target func(map[string]string) bool
	NodeID string
}

// Compile turns rule specs into executable rules.
func Compile(rules []model.InhibitRule, nodeID string) ([]CompiledRule, error) {
	out := make([]CompiledRule, 0, len(rules))
	for _, r := range rules {
		src, err := routing.CompileMatchers(r.Source)
		if err != nil {
			return nil, fmt.Errorf("inhibit %s source: %w", r.ID, err)
		}
		tgt, err := routing.CompileMatchers(r.Target)
		if err != nil {
			return nil, fmt.Errorf("inhibit %s target: %w", r.ID, err)
		}
		out = append(out, CompiledRule{Spec: r, Source: src, Target: tgt, NodeID: nodeID})
	}
	return out, nil
}

// Decision says whether an alert is inhibited at an instant and why.
type Decision struct {
	Inhibited bool
	RuleID    string
	SourceIDs []string
	Missing   string // the equal label that did not match, when informative
}

// Evaluate checks one alert against all rules, given the set of currently
// firing alerts. An alert can inhibit itself only through a rule whose source
// and target both match it and which has at least one Equal label — that is
// meaningless, so self-inhibition is ignored.
func Evaluate(rules []CompiledRule, alert model.Alert, firing []model.Alert) Decision {
	for _, r := range rules {
		if !r.Target(alert.Labels) {
			continue
		}
		var sources []string
		for _, f := range firing {
			if f.ID == alert.ID {
				continue
			}
			if !r.Source(f.Labels) {
				continue
			}
			if equalLabels(r.Spec.Equal, alert.Labels, f.Labels) {
				sources = append(sources, f.ID)
			}
		}
		if len(sources) > 0 {
			return Decision{Inhibited: true, RuleID: r.Spec.ID, SourceIDs: sources}
		}
	}
	return Decision{}
}

// equalLabels reports whether both label sets agree on every listed label.
// A label missing from either side counts as empty on both sides.
func equalLabels(names []string, a, b map[string]string) bool {
	for _, n := range names {
		if a[n] != b[n] {
			return false
		}
	}
	return true
}

// Explain describes a rule for display.
func Explain(r CompiledRule) string {
	return fmt.Sprintf("%s → %s 需相同标签 %v", routing.DescribeMatch(r.Spec.Source), routing.DescribeMatch(r.Spec.Target), r.Spec.Equal)
}
