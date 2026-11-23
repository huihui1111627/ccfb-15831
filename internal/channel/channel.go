// Package channel simulates notification transport: per-replica staggered
// sending, exponential-backoff retries, injected channel faults, and replica
// knowledge propagation that is cut by network partitions.
package channel

import (
	"fmt"
	"sort"
	"time"

	"kilnalert/internal/model"
)

// Config describes one simulated channel.
type Config struct {
	Name       string
	Stagger    time.Duration // wait after the logical time before the first replica tries
	ReplicaGap time.Duration // extra wait per replica index, so replicas try in order
	// FailRounds makes every notification fail for its first FailRounds
	// attempt rounds (a channel outage). FailEvery additionally fails the
	// notifications whose identity hashes into every Nth bucket, for their
	// first three rounds — a permanently flaky slice of the traffic.
	FailRounds int
	FailEvery  int
	MaxRounds  int // attempt rounds before giving up
}

// Backoff is exponential retry: Base, 2*Base, 4*Base ... capped at Max.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay returns the wait before retry number attempt (0-based).
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	d := base
	for i := 0; i < attempt; i++ {
		if b.Max > 0 && d >= b.Max {
			return b.Max
		}
		d *= 2
	}
	if b.Max > 0 && d > b.Max {
		return b.Max
	}
	return d
}

// Network records partition intervals between replicas.
type Network struct {
	Parts []Partition
}

// Partition isolates replica groups from each other during [From,To).
type Partition struct {
	From   time.Time
	To     time.Time
	Groups [][]string
}

// Connected reports whether two replicas can exchange knowledge at t.
func (n *Network) Connected(a, b string, t time.Time) bool {
	if a == b {
		return true
	}
	for _, p := range n.Parts {
		if t.Before(p.From) || !t.Before(p.To) {
			continue
		}
		return groupOf(p, a) == groupOf(p, b)
	}
	return true
}

// PartitionedAt reports whether any partition is in force at t.
func (n *Network) PartitionedAt(t time.Time) bool {
	for _, p := range n.Parts {
		if !t.Before(p.From) && t.Before(p.To) {
			return true
		}
	}
	return false
}

func groupOf(p Partition, r string) int {
	for i, g := range p.Groups {
		for _, x := range g {
			if x == r {
				return i
			}
		}
	}
	return -1
}

// Dispatcher sends notifications from replicas to channels.
type Dispatcher struct {
	Replicas []string
	Channels map[string]Config
	Backoff  Backoff
	Net      *Network
	// CurrentChannel returns the channel a node uses at t; a notification
	// whose channel is no longer current is abandoned rather than retried.
	CurrentChannel func(nodeID string, t time.Time) string

	knowledge map[string]map[string]bool // replica -> delivered dedupe ids
	// Delivered records successful sends per dedupe id, for duplicate stats.
	amAttempts map[string]int
	Deliveries []Delivery
	Stats      Stats
}

// Delivery is one attempt's outcome.
type Delivery struct {
	NotificationID int
	DedupeID       string
	Channel        string
	Replica        string
	ReplicaIdx     int
	Round          int
	At             time.Time
	OK             bool
	Duplicate      bool
	Held           bool // skipped because a peer was known to have sent it
	Abandoned      bool // channel was switched away
	Reason         string
	Backoff        time.Duration
}

// Stats sums dispatcher activity.
type Stats struct {
	Attempts       int
	Failures       int
	Delivered      int
	Duplicates     int
	HeldDuplicates int
	Abandoned      int
	Retries        int
}

// NewDispatcher builds a dispatcher.
func NewDispatcher(replicas []string, chans []Config, b Backoff, net *Network, current func(string, time.Time) string) *Dispatcher {
	d := &Dispatcher{
		Replicas:       replicas,
		Channels:       map[string]Config{},
		Backoff:        b,
		Net:            net,
		CurrentChannel: current,
		knowledge:      map[string]map[string]bool{},
		amAttempts:     map[string]int{},
	}
	for _, c := range chans {
		if c.MaxRounds <= 0 {
			c.MaxRounds = 8
		}
		d.Channels[c.Name] = c
	}
	for _, r := range replicas {
		d.knowledge[r] = map[string]bool{}
	}
	return d
}

// visibleTo returns the union of knowledge of every replica connected to r at t.
func (d *Dispatcher) visibleTo(r string, t time.Time) map[string]bool {
	out := map[string]bool{}
	for peer, set := range d.knowledge {
		if !d.Net.Connected(r, peer, t) {
			continue
		}
		for id := range set {
			out[id] = true
		}
	}
	return out
}

// Schedule returns delivery attempts for all notifications, in time order.
// It is pure with respect to the notification list; call Run to execute.
type attempt struct {
	at      time.Time
	notif   int
	replica string
	round   int
}

// Runs returns the scheduled attempts for one notification.
func (d *Dispatcher) scheduleFor(n model.Notification) []attempt {
	ch, ok := d.Channels[n.Channel]
	if !ok {
		return nil
	}
	var out []attempt
	base := n.LogicalAt.Add(ch.Stagger)
	for round := 0; round < ch.MaxRounds; round++ {
		for i, r := range d.Replicas {
			at := base.Add(time.Duration(i) * ch.ReplicaGap)
			out = append(out, attempt{at: at, notif: n.ID, replica: r, round: round})
		}
		// Next round starts after the last replica of this round, plus the
		// backoff delay for the failures seen in it.
		last := base.Add(time.Duration(len(d.Replicas)-1) * ch.ReplicaGap)
		base = last.Add(d.Backoff.Delay(round))
	}
	return out
}

// Run executes all attempts for the notifications. Notifications must be
// sorted by LogicalAt, ID. It returns deliveries in chronological order.
func (d *Dispatcher) Run(notifs []model.Notification) []Delivery {
	type ev struct {
		at      time.Time
		notif   int
		replica string
		round   int
		idx     int
		order   int
	}
	var evs []ev
	for _, n := range notifs {
		for _, a := range d.scheduleFor(n) {
			idx := 0
			for i, r := range d.Replicas {
				if r == a.replica {
					idx = i
				}
			}
			evs = append(evs, ev{at: a.at, notif: a.notif, replica: a.replica, round: a.round, idx: idx, order: len(evs)})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].at.Equal(evs[j].at) {
			return evs[i].order < evs[j].order
		}
		return evs[i].at.Before(evs[j].at)
	})
	byID := map[int]model.Notification{}
	for _, n := range notifs {
		byID[n.ID] = n
	}
	delivered := map[string]bool{}
	for _, e := range evs {
		n := byID[e.notif]
		id := n.DedupeID()
		if delivered[id] && e.round > 0 {
			// The notification was delivered in an earlier round; stop.
			continue
		}
		rec := Delivery{NotificationID: n.ID, DedupeID: id, Channel: n.Channel, Replica: e.replica, ReplicaIdx: e.idx, Round: e.round, At: e.at}
		ch := d.Channels[n.Channel]
		cur := n.Channel
		if d.CurrentChannel != nil {
			cur = d.CurrentChannel(n.NodeID, e.at)
		}
		if cur != n.Channel {
			rec.Abandoned = true
			rec.Reason = "组已切换到渠道 " + cur + "，旧渠道不再重试"
			d.Stats.Abandoned++
			d.Deliveries = append(d.Deliveries, rec)
			continue
		}
		vis := d.visibleTo(e.replica, e.at)
		if vis[id] {
			rec.Held = true
			rec.Reason = "其他副本已发送，按去重不再发送"
			d.Stats.HeldDuplicates++
			d.Deliveries = append(d.Deliveries, rec)
			continue
		}
		d.amAttempts[id]++
		rec.Reason = ""
		d.Stats.Attempts++
		if e.round > 0 {
			rec.Backoff = d.Backoff.Delay(e.round - 1)
			d.Stats.Retries++
		}
		if d.faulty(ch, id, e.round) {
			rec.Reason = "渠道故障（模拟注入）"
			rec.Backoff = d.Backoff.Delay(e.round)
			d.Stats.Failures++
			d.Deliveries = append(d.Deliveries, rec)
			continue
		}
		rec.OK = true
		d.Stats.Delivered++
		if delivered[id] {
			// Another replica already delivered this notification but is cut
			// off from this one: a deliberate duplicate, preferred over a loss.
			rec.Duplicate = true
			d.Stats.Duplicates++
		}
		delivered[id] = true
		d.knowledge[e.replica][id] = true
		d.Deliveries = append(d.Deliveries, rec)
	}
	return d.Deliveries
}

// Undelivered returns the dedupe ids of notifications that never reached a
// channel, excluding ones abandoned because of a channel switch.
func (d *Dispatcher) Undelivered(notifs []model.Notification) []string {
	ok := map[string]bool{}
	for _, del := range d.Deliveries {
		if del.OK {
			ok[del.DedupeID] = true
		}
	}
	var out []string
	switchedAway := map[string]bool{}
	for _, del := range d.Deliveries {
		if del.Abandoned {
			switchedAway[del.DedupeID] = true
		}
	}
	for _, n := range notifs {
		if !ok[n.DedupeID()] && !switchedAway[n.DedupeID()] {
			out = append(out, n.DedupeID())
		}
	}
	sort.Strings(out)
	return out
}

// DescribeChannel renders a channel config.
func DescribeChannel(c Config) string {
	s := "错峰 " + c.Stagger.String()
	if c.ReplicaGap > 0 {
		s += "，副本间隔 " + c.ReplicaGap.String()
	}
	if c.FailRounds > 0 {
		s += fmt.Sprintf("，前 %d 轮重试前注入故障", c.FailRounds)
	}
	if c.FailEvery > 0 {
		s += fmt.Sprintf("，%d 分之一的通知持续故障三轮", c.FailEvery)
	}
	return s
}

// faulty reports whether this attempt is one the simulation injects a
// failure into. The decision depends only on the notification and the round,
// so all replicas see the same channel behaviour.
func (d *Dispatcher) faulty(c Config, dedupe string, round int) bool {
	if round < c.FailRounds {
		return true
	}
	if c.FailEvery > 0 && round < 3 {
		h := uint32(2166136261)
		for i := 0; i < len(dedupe); i++ {
			h ^= uint32(dedupe[i])
			h *= 16777619
		}
		if int(h%uint32(c.FailEvery)) == 0 {
			return true
		}
	}
	return false
}
