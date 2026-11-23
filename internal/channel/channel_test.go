package channel

import (
	"testing"
	"time"

	"kilnalert/internal/model"
)

func n(id int, at time.Time) model.Notification {
	return model.Notification{ID: id, Key: "k", NodeID: "n", Channel: "sms", LogicalAt: at, Ordinal: id}
}

func on(ch string, id int, at time.Time) model.Notification {
	return model.Notification{ID: id, Key: "k", NodeID: "n", Channel: ch, LogicalAt: at, Ordinal: id}
}

func TestStaggeredReplicaOrderSendsOnce(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	net := &Network{}
	d := NewDispatcher([]string{"r0", "r1", "r2"}, []Config{{Name: "sms", Stagger: 30 * time.Second, ReplicaGap: 10 * time.Second}},
		Backoff{Base: time.Second}, net, func(string, time.Time) string { return "sms" })
	d.Run([]model.Notification{n(1, t0)})
	if len(d.Deliveries) == 1 {
		// Exactly one attempt is recorded before the others are held.
	}
	ok := 0
	for _, x := range d.Deliveries {
		if x.OK {
			ok++
			if x.Replica != "r0" {
				t.Fatalf("expected r0 to win, got %s", x.Replica)
			}
		}
	}
	if ok != 1 {
		t.Fatalf("expected exactly one delivery, got %d", ok)
	}
	if d.Stats.HeldDuplicates != 2 {
		t.Fatalf("expected 2 held, got %d", d.Stats.HeldDuplicates)
	}
}

func TestPartitionAllowsDuplicate(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	net := &Network{Parts: []Partition{{From: t0, To: t0.Add(time.Hour), Groups: [][]string{{"r0"}, {"r1", "r2"}}}}}
	d := NewDispatcher([]string{"r0", "r1", "r2"}, []Config{{Name: "sms", Stagger: 10 * time.Second, ReplicaGap: 5 * time.Second}},
		Backoff{Base: time.Second}, net, func(string, time.Time) string { return "sms" })
	d.Run([]model.Notification{n(1, t0)})
	delivered := map[string]bool{}
	for _, x := range d.Deliveries {
		if x.OK {
			delivered[x.Replica] = true
		}
	}
	if !delivered["r0"] || !delivered["r1"] {
		t.Fatalf("partition should allow both groups to send, got %v", delivered)
	}
	if delivered["r2"] {
		t.Fatalf("r2 and r1 share a group and should not both send")
	}
	if d.Stats.Duplicates == 0 {
		t.Fatal("expected a duplicate to be counted")
	}
}

func TestBackoff(t *testing.T) {
	b := Backoff{Base: 2 * time.Second, Max: 10 * time.Second}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := b.Delay(i); got != w {
			t.Errorf("attempt %d: got %v want %v", i, got, w)
		}
	}
}

func TestRetryUntilSuccess(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	net := &Network{}
	d := NewDispatcher([]string{"r0"}, []Config{{Name: "email", Stagger: 0, ReplicaGap: 0, FailRounds: 2, MaxRounds: 5}},
		Backoff{Base: time.Minute}, net, func(string, time.Time) string { return "email" })
	d.Run([]model.Notification{on("email", 1, t0)})
	last := d.Deliveries[len(d.Deliveries)-1]
	if !last.OK {
		t.Fatalf("expected eventual success, deliveries: %+v", d.Deliveries)
	}
	if last.Round != 2 {
		t.Fatalf("expected success on round 2 (third attempt), got round %d", last.Round)
	}
}

func TestChannelSwitchAbandons(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	net := &Network{}
	sw := t0.Add(-time.Second)
	d := NewDispatcher([]string{"r0"}, []Config{{Name: "sms", Stagger: 0, ReplicaGap: 0}},
		Backoff{Base: time.Second}, net, func(string, time.Time) string { return "email" })
	_ = sw
	d.Run([]model.Notification{n(1, t0)})
	for _, x := range d.Deliveries {
		if x.OK {
			t.Fatal("abandoned notification must not be delivered to the old channel")
		}
		if !x.Abandoned {
			t.Fatalf("expected abandoned, got %+v", x)
		}
	}
	if len(d.Undelivered([]model.Notification{n(1, t0)})) != 0 {
		t.Fatal("an abandoned notification is not an undelivered one")
	}
}
