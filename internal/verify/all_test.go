package verify_test

import (
	"testing"

	"kilnalert/internal/sim"
)

// TestAllStormsPass is the acceptance test: every storm's emitted
// notifications must pair one-for-one with the ones derived from the rules and
// the complete alert record, the only duplicates allowed are inside
// partitions, and every notification must have reached a channel.
func TestAllStormsPass(t *testing.T) {
	results, err := sim.RunAll(sim.Storms())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(sim.Storms()) {
		t.Fatalf("expected %d storms, got %d", len(sim.Storms()), len(results))
	}
	for _, r := range results {
		if problems := r.Verify.Problems(); len(problems) > 0 {
			for i, p := range problems {
				if i == 5 {
					t.Logf("...%d more problems", len(problems)-i)
					break
				}
				t.Errorf("%s: %s", r.Storm.Name, p)
			}
		}
		if r.Verify.Expected == 0 && r.Storm.Name != "" {
			// A storm with no notifications would make the test vacuous.
			t.Errorf("%s: no notifications expected; the scenario is not exercising the pipeline", r.Storm.Name)
		}
	}
}
