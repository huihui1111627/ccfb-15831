package sim

import "testing"

// TestRunsAreReproducible pins the whole pipeline: the same storm at the same
// seed must produce byte-identical notification payloads.
func TestRunsAreReproducible(t *testing.T) {
	for _, name := range []string{"kiln-blackout", "partition-during-flood", "all-kinds"} {
		var st Storm
		for _, s := range Storms() {
			if s.Name == name {
				st = s
			}
		}
		a, err := Run(st)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Run(st)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Run.Notifications) != len(b.Run.Notifications) {
			t.Fatalf("%s: %d then %d notifications", name, len(a.Run.Notifications), len(b.Run.Notifications))
		}
		for i := range a.Run.Notifications {
			x, y := a.Run.Notifications[i], b.Run.Notifications[i]
			if x.DedupeID() != y.DedupeID() || len(x.Delta.Added) != len(y.Delta.Added) || len(x.Delta.Resolved) != len(y.Delta.Resolved) {
				t.Fatalf("%s: notification %d differs between runs", name, i)
			}
			for j := range x.Delta.Added {
				if x.Delta.Added[j].ID != y.Delta.Added[j].ID {
					t.Fatalf("%s: notification %d addition %d differs", name, i, j)
				}
			}
		}
	}
}

// TestPartitionDuplicatesAreBounded checks the "prefer a duplicate to a loss"
// policy is bounded: no notification goes out more than once per replica.
func TestPartitionDuplicatesAreBounded(t *testing.T) {
	var st Storm
	for _, s := range Storms() {
		if s.Name == "partition-during-flood" {
			st = s
		}
	}
	res, err := Run(st)
	if err != nil {
		t.Fatal(err)
	}
	byNotif := map[int]map[string]bool{}
	for _, d := range res.Run.Deliveries {
		if !d.OK {
			continue
		}
		if byNotif[d.NotificationID] == nil {
			byNotif[d.NotificationID] = map[string]bool{}
		}
		if byNotif[d.NotificationID][d.Replica] {
			t.Fatalf("notification %d delivered twice by replica %s", d.NotificationID, d.Replica)
		}
		byNotif[d.NotificationID][d.Replica] = true
	}
	if res.Verify.DuplicateDeliveries == 0 {
		t.Fatal("this storm should produce partition duplicates")
	}
}
