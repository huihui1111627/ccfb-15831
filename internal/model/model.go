// Package model holds the shared data types: alerts, routing configuration,
// generated notifications and the audit trail the service produces.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Status is the lifecycle state of an alert instance.
type Status string

// Instance states. Pending is currently unused but reserved for alerts held
// back by a future policy.
const (
	StatusFiring   Status = "firing"
	StatusResolved Status = "resolved"
)

// Alert is one alert instance. Instances are identified by their label set;
// a resolve and a later re-fire of the same labels are two episodes of the
// same instance ID.
type Alert struct {
	ID       string
	Labels   map[string]string
	Status   Status
	Start    time.Time
	End      time.Time // resolve or auto-resolve time; zero while firing
	LastSeen time.Time // last time a source reported it firing
}

// Fingerprint is the identity of an alert: a hash of its sorted label set.
func Fingerprint(labels map[string]string) string {
	keys := sortedKeys(labels)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\x00')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Clone returns a copy of the alert and its labels.
func (a Alert) Clone() Alert {
	c := a
	c.Labels = CloneLabels(a.Labels)
	return c
}

// CloneLabels copies a label map.
func CloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// String renders labels deterministically, for display.
func String(labels map[string]string) string {
	keys := sortedKeys(labels)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

// Matcher tests a single label.
type Matcher struct {
	Name  string `json:"name"`
	Op    string `json:"op"` // =, !=, =~, !~
	Value string `json:"value"`
}

// MatchAll is a label matcher set: every matcher must match. An empty set
// matches every alert.
type MatchAll []Matcher

// TimeWindow is an allowed time of day in a specific timezone. It may wrap
// midnight (Start > End). Days restricts it to those weekdays (0=Sunday; an
// empty list means every day). Holidays, when non-nil, restricts it to those
// dates in the window's timezone.
type TimeWindow struct {
	Start        time.Duration
	End          time.Duration
	Days         []time.Weekday
	LocationName string
	Location     *time.Location
	Holidays     map[string]bool
}

// InhibitRule mutes targets while a source matching Source is firing, when
// both sides agree on every label in Equal.
type InhibitRule struct {
	ID     string   `json:"id"`
	Source MatchAll `json:"source"`
	Target MatchAll `json:"target"`
	Equal  []string `json:"equal"`
	Note   string   `json:"note,omitempty"`
}

// RouteNode is one node of the routing tree.
type RouteNode struct {
	ID               string
	Name             string
	Parent           string
	Match            MatchAll
	GroupBy          []string
	ContinueSiblings bool // after a match, still try the following siblings
	TimeWindows      []TimeWindow
	Channel          string // channel for alerts routed here
	InhibitRules     []InhibitRule
	Silences         []*Silence
	Children         []*RouteNode
}

// Silence suppresses alerts matching Match during [Starts, Ends), unless
// cancelled first.
type Silence struct {
	ID          string
	Match       MatchAll
	Starts      time.Time
	Ends        time.Time
	CancelledAt time.Time
	ActiveStart time.Time
	ActiveEnd   time.Time
	Reason      string
	NodeID      string // empty: applies to every node
	CreatedAt   time.Time
}

// ActiveAt reports the silence's effective state at t from the cluster's
// point of view (leader replica). Individual replicas may lag; the engine
// evaluates from the leader's knowledge.
func (s *Silence) ActiveAt(t time.Time) bool {
	if t.Before(s.Starts) || !t.Before(s.Ends) {
		return false
	}
	if !s.CancelledAt.IsZero() && !s.CancelledAt.After(t) {
		return false
	}
	return true
}

// GroupKey identifies one group of alerts in a route node.
type GroupKey struct {
	NodeID   string
	LabelSet []string
	Values   []string
}

// Key is a canonical string for the group.
func (g GroupKey) Key() string {
	return g.NodeID + "\x00" + strings.Join(g.LabelSet, ",") + "\x00" + strings.Join(g.Values, "\x00")
}

// AlertRecord is the snapshot receivers are told about an alert.
type AlertRecord struct {
	ID     string
	Labels map[string]string
	Start  time.Time
	End    time.Time
	Status Status
}

// Delta is the change of a group since the previous notification to a channel.
type Delta struct {
	Added    []AlertRecord
	Resolved []AlertRecord
}

// SuppressedNote is one alert held back from a notification, with the reason.
type SuppressedNote struct {
	AlertID   string
	Reason    string // "silence" or "inhibit"
	RuleID    string
	SilenceID string
	Sources   []string
}

// Notification is one logical message to one channel for one group. Its
// content is fixed at generation: retries re-send it verbatim.
type Notification struct {
	ID          int
	Key         string
	NodeID      string
	LabelNames  []string
	GroupValues []string
	Channel     string
	Delta       Delta
	// Suppressed lists alerts in the group that were held back from this
	// notification, with the reason. It is informational: receivers are not
	// expected to act on it.
	Suppressed []SuppressedNote
	LogicalAt  time.Time
	Ordinal    int  // per (group, channel) sequence number
	Baseline   bool // first notification to this channel: full state as added
}

// DedupeID identifies this notification across replicas.
func (n Notification) DedupeID() string {
	return DedupeIDFor(n.LogicalAt, n.Key, n.Channel, n.Ordinal)
}

// DedupeIDFor builds the cross-replica dedupe key. The ordinal is included so
// two notifications for the same group and channel in the same tick cannot
// collide.
func DedupeIDFor(at time.Time, key, channel string, ordinal int) string {
	return at.UTC().Format(time.RFC3339Nano) + "|" + key + "|" + channel + "|" + itoa(ordinal)
}

func itoa(x int) string {
	if x == 0 {
		return "0"
	}
	neg := x < 0
	if neg {
		x = -x
	}
	var b [20]byte
	i := len(b)
	for x > 0 {
		i--
		b[i] = byte('0' + x%10)
		x /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Delivery is one send attempt recorded by the channel simulation.
type Delivery struct {
	NotificationID int
	DedupeID       string
	Channel        string
	Replica        string
	ReplicaIdx     int
	Attempt        int
	At             time.Time
	OK             bool
	Duplicate      bool
	Reason         string
	Backoff        time.Duration
}

// AlertEvent is one observation reported by a simulated source.
type AlertEvent struct {
	Labels map[string]string
	Fire   bool // true: firing/continuing; false: explicit resolve
	At     time.Time
	Source string
	Note   string
}

// Working is an alert instance during processing.
type Working struct {
	ID       string
	Labels   map[string]string
	Status   Status
	Start    time.Time
	End      time.Time
	LastSeen time.Time
	Episodes int
}

// SilenceView is a silence with its display state.
type SilenceView struct {
	Silence
	State string
}

// NodeStats aggregates a node's contribution to a run.
type NodeStats struct {
	Alerts        int
	Notifications int
	Suppressed    int
}

// RunStats is the compression and reliability summary.
type RunStats struct {
	SourceEvents      int
	AlertInstances    int
	Notifications     int
	Deliveries        int
	Duplicates        int
	SuppressedSilence int
	SuppressedInhibit int
	HeldDuplicates    int
	NetworkFailures   int
	Retries           int
	ChannelSwitches   int
}

// RunResult is one scenario's complete output.
type RunResult struct {
	Name          string
	Seed          int64
	Start         time.Time
	End           time.Time
	Replicas      []string
	Alerts        []Alert
	AlertEvents   []AlertEvent
	Notifications []Notification
	Deliveries    []Delivery
	Silences      []SilenceView
	Events        []TimelineEvent
	Stats         RunStats
	Problems      []string
	Tree          *RouteNode
	Channels      []string
	Partitions    []Partition
	Batching      BatchParams
	Switches      []SwitchInfo
	Snapshots     []Snapshot
}

// BatchParams are the batching rules, recorded for replay.
type BatchParams struct {
	Step          time.Duration
	GroupWait     time.Duration
	GroupInterval time.Duration
	StaleAfter    time.Duration
}

// SwitchInfo records a channel switch.
type SwitchInfo struct {
	NodeID string
	At     time.Time
	To     string
}

// Partition records a replica partition.
type Partition struct {
	From   time.Time
	To     time.Time
	Groups [][]string
}

// GroupSnapshot is one group's state at a tick.
type GroupSnapshot struct {
	NodeID     string
	LabelSet   []string
	Values     []string
	Channel    string
	Firing     []string // alert IDs, sorted
	Suppressed []SuppressedNote
	LastSent   time.Time
	Pending    bool
}

// Snapshot is the pipeline's state at one tick, recorded for display.
type Snapshot struct {
	At            time.Time
	Alerts        []Alert
	Groups        []GroupSnapshot
	SilenceActive []bool
	Notifications int // notifications emitted up to and including this tick
}

// TimelineEvent is a notable event, for the activity page.
type TimelineEvent struct {
	At      time.Time
	Kind    string
	Text    string
	AlertID string
	NodeID  string
	Key     string
}
