// Package engine is the alert pipeline.
//
// Time advances in fixed ticks. At each tick the engine
//
//  1. auto-resolves firing alerts whose last source report is older than
//     StaleAfter,
//  2. routes every firing alert through the routing tree,
//  3. decides per group whether the alert is silenced or inhibited,
//  4. emits, for each group and its current channel, a notification carrying
//     only the change since that channel's previous notification.
//
// Batching rule (the same rule the verifier replays): a group's first
// notification to a channel waits GroupWait after the group first has
// something to report; later ones are spaced at least GroupInterval apart.
// Notification content at a tick is
//
//	added    = firing and unsuppressed and routed here and not known to the channel
//	resolved = known to the channel but no longer firing, or no longer routed here
//
// Suppression never produces a resolution: an alert that was notified and is
// then inhibited or silenced simply stops changing until it genuinely
// resolves. This keeps "源告警在触发时目标告警不发通知" true without telling
// receivers the target stopped firing when it did not.
package engine

import (
	"fmt"
	"sort"
	"time"

	"kilnalert/internal/inhibit"
	"kilnalert/internal/model"
	"kilnalert/internal/routing"
)

// ChannelSwitch moves a node's notifications to another channel from At on.
// Notifications already generated for the old channel but not yet delivered
// are abandoned, not retried there; the new channel starts with a baseline.
type ChannelSwitch struct {
	NodeID string
	At     time.Time
	To     string
}

// Config tunes the pipeline.
type Config struct {
	Step          time.Duration
	GroupWait     time.Duration
	GroupInterval time.Duration
	StaleAfter    time.Duration
	// PendingHold holds an alert's first notification until the alert has
	// gone PendingHold without a source report, so a probe that resolves
	// within that window never notifies at all.
	PendingHold time.Duration
	Replicas    []string
	Switches    []ChannelSwitch
}

// Decision is the suppression verdict for one alert in one group.
type Decision struct {
	Reason    string // "silence" or "inhibit"; empty when not suppressed
	SilenceID string
	RuleID    string
	Sources   []string
}

// Known is what one channel has been told about a group.
type Known struct {
	Alerts  map[string]model.AlertRecord
	LastAt  time.Time
	Ordinal int
}

// Group is the engine's live view of one routed group.
type Group struct {
	Node         *routing.Node
	Key          model.GroupKey
	Desired      map[string]bool // firing alerts routed here, suppressed or not
	Suppressed   map[string]Decision
	Known        map[string]*Known // by channel
	PendingSince map[string]time.Time
}

// KeyString is the group's canonical key.
func (g *Group) KeyString() string { return g.Key.Key() }

// Engine is one simulation's pipeline state.
type Engine struct {
	cfg       Config
	tree      *routing.Tree
	nodeRules map[string][]inhibit.CompiledRule

	alerts   map[string]*model.Working
	byFP     map[string]string
	episodes map[string]int
	groups   map[string]*Group
	silences []*model.Silence

	notifs        []model.Notification
	observed      []model.AlertEvent
	snapshots     []model.Snapshot
	everSilenced  map[string]bool
	everInhibited map[string]bool
	timeline      []model.TimelineEvent
	stats         model.RunStats
	nextID        int
}

// New builds an engine over a compiled tree.
func New(cfg Config, tree *routing.Tree, t0, tEnd time.Time) *Engine {
	if cfg.Step <= 0 {
		cfg.Step = 10 * time.Second
	}
	e := &Engine{
		cfg:           cfg,
		tree:          tree,
		alerts:        map[string]*model.Working{},
		byFP:          map[string]string{},
		episodes:      map[string]int{},
		everSilenced:  map[string]bool{},
		everInhibited: map[string]bool{},
		groups:        map[string]*Group{},
		nodeRules:     map[string][]inhibit.CompiledRule{},
	}
	for _, n := range tree.Order {
		var rules []inhibit.CompiledRule
		if n.Parent != nil {
			rules = append(rules, e.nodeRules[n.Parent.Spec.ID]...)
		}
		if len(n.Spec.InhibitRules) > 0 {
			if own, err := inhibit.Compile(n.Spec.InhibitRules, n.Spec.ID); err == nil {
				rules = append(rules, own...)
			}
		}
		e.nodeRules[n.Spec.ID] = rules
	}
	return e
}

func (e *Engine) log(at time.Time, kind, text, alertID, nodeID string) {
	e.timeline = append(e.timeline, model.TimelineEvent{At: at, Kind: kind, Text: text, AlertID: alertID, NodeID: nodeID})
}

// Ingest applies one source observation. Observations for a tick must arrive
// before that tick.
//
// An alert's identity is its label set plus its episode: a fingerprint that
// resolves and later fires again is a new episode with a new alert ID, so
// receivers see the old one resolve and the new one arrive as an addition,
// even if the second episode starts before the first resolution was sent.
func (e *Engine) Ingest(ev model.AlertEvent) {
	e.observed = append(e.observed, model.AlertEvent{Labels: model.CloneLabels(ev.Labels), Fire: ev.Fire, At: ev.At, Source: ev.Source, Note: ev.Note})
	fp := model.Fingerprint(ev.Labels)
	id := e.byFP[fp]
	if ev.Fire {
		e.stats.SourceEvents++
		if id == "" {
			e.episodes[fp]++
			id = episodeID(fp, e.episodes[fp])
			e.alerts[id] = &model.Working{
				ID: id, Labels: model.CloneLabels(ev.Labels), Status: model.StatusFiring,
				Start: ev.At, LastSeen: ev.At, Episodes: e.episodes[fp],
			}
			e.byFP[fp] = id
			e.stats.AlertInstances++
			e.log(ev.At, "alert_start", "触发 "+model.String(ev.Labels), id, "")
			return
		}
		w := e.alerts[id]
		w.LastSeen = ev.At
		return
	}
	e.stats.SourceEvents++
	if id == "" {
		e.log(ev.At, "alert_resolve", "解除未知告警（忽略） "+model.String(ev.Labels), "", "")
		return
	}
	w := e.alerts[id]
	if w.Status != model.StatusFiring {
		return
	}
	w.Status = model.StatusResolved
	w.End = ev.At
	w.LastSeen = ev.At
	delete(e.byFP, fp)
	e.log(ev.At, "alert_resolve", "解除 "+model.String(ev.Labels), id, "")
}

// episodeID names the nth firing episode of a label set.
func episodeID(fp string, n int) string {
	if n <= 1 {
		return fp
	}
	return fp + "#" + itoa(n)
}

func itoa(x int) string {
	if x == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for x > 0 {
		i--
		b[i] = byte('0' + x%10)
		x /= 10
	}
	return string(b[i:])
}

// AddSilence registers a silence.
func (e *Engine) AddSilence(s *model.Silence) {
	if !s.Ends.After(s.Starts) {
		return
	}
	e.silences = append(e.silences, s)
}

// CancelSilence ends a silence at at.
func (e *Engine) CancelSilence(id string, at time.Time) bool {
	for _, s := range e.silences {
		if s.ID != id || !s.CancelledAt.IsZero() {
			continue
		}
		s.CancelledAt = at
		if at.Before(s.Starts) {
			e.log(at, "silence_cancel", "静默在生效前被取消 "+routing.Describe(s.Match), "", s.NodeID)
		} else {
			if s.ActiveStart.IsZero() {
				s.ActiveStart = s.Starts
			}
			s.ActiveEnd = at
			e.log(at, "silence_cancel", "静默提前取消 "+routing.Describe(s.Match), "", s.NodeID)
		}
		return true
	}
	return false
}

// SilenceActive reports whether s suppresses at t. Shared by engine, display
// and verifier.
func SilenceActive(s *model.Silence, t time.Time) bool {
	if t.Before(s.Starts) || !t.Before(s.Ends) {
		return false
	}
	return s.CancelledAt.IsZero() || t.Before(s.CancelledAt)
}

// Tick advances to t; ticks must be increasing.
func (e *Engine) Tick(t time.Time) {
	e.autoResolveStale(t)
	e.markSilenceLifecycle(t)
	e.reconcile(t)
	e.emit(t)
	e.stats.SuppressedSilence = len(e.everSilenced)
	e.stats.SuppressedInhibit = len(e.everInhibited)
	e.snapshot(t)
}

// snapshot records the display state at a tick.
func (e *Engine) snapshot(t time.Time) {
	sn := model.Snapshot{At: t, Notifications: len(e.notifs)}
	ids := make([]string, 0, len(e.alerts))
	for id := range e.alerts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := e.alerts[id]
		sn.Alerts = append(sn.Alerts, model.Alert{
			ID: id, Labels: w.Labels, Status: w.Status, Start: w.Start, End: w.End, LastSeen: w.LastSeen,
		})
	}
	for _, g := range e.GroupsSorted() {
		gs := model.GroupSnapshot{
			NodeID: g.Node.Spec.ID, LabelSet: append([]string{}, g.Key.LabelSet...),
			Values:  append([]string{}, g.Key.Values...),
			Channel: e.currentChannel(g.Node.Spec.ID, g.Node.Spec.Channel, t),
		}
		for id := range g.Desired {
			if _, sup := g.Suppressed[id]; sup {
				continue
			}
			gs.Firing = append(gs.Firing, id)
		}
		sort.Strings(gs.Firing)
		gs.Suppressed = e.suppressedNotes(g)
		if k := g.Known[gs.Channel]; k != nil {
			gs.LastSent = k.LastAt
			gs.Pending = len(g.PendingSince) > 0
			if _, ok := g.PendingSince[gs.Channel]; !ok {
				gs.Pending = false
			}
		}
		sn.Groups = append(sn.Groups, gs)
	}
	for _, s := range e.silences {
		sn.SilenceActive = append(sn.SilenceActive, SilenceActive(s, t))
	}
	e.snapshots = append(e.snapshots, sn)
}

// Snapshots returns the recorded states.
func (e *Engine) Snapshots() []model.Snapshot { return e.snapshots }

func (e *Engine) autoResolveStale(t time.Time) {
	ids := make([]string, 0, len(e.alerts))
	for id := range e.alerts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := e.alerts[id]
		if w.Status != model.StatusFiring {
			continue
		}
		deadline := w.LastSeen.Add(e.cfg.StaleAfter)
		if t.Before(deadline) {
			continue
		}
		w.Status = model.StatusResolved
		w.End = deadline
		delete(e.byFP, model.Fingerprint(w.Labels))
		e.log(deadline, "auto_resolve", "重发超时，判定解除 "+model.String(w.Labels), id, "")
	}
}

func (e *Engine) markSilenceLifecycle(t time.Time) {
	for _, s := range e.silences {
		if s.ActiveStart.IsZero() && !t.Before(s.Starts) && (s.CancelledAt.IsZero() || s.CancelledAt.After(t)) {
			s.ActiveStart = s.Starts
			e.log(s.Starts, "silence_activate", "静默生效 "+routing.Describe(s.Match), "", s.NodeID)
		}
		if s.ActiveStart.IsZero() {
			continue
		}
		if s.ActiveEnd.IsZero() && !t.Before(s.Ends) {
			s.ActiveEnd = s.Ends
			e.log(s.Ends, "silence_expire", "静默到期自动失效 "+routing.Describe(s.Match), "", s.NodeID)
		}
	}
}

func (e *Engine) firingSnapshot() []model.Alert {
	out := make([]model.Alert, 0, len(e.alerts))
	for _, w := range e.alerts {
		if w.Status != model.StatusFiring {
			continue
		}
		out = append(out, model.Alert{ID: w.ID, Labels: w.Labels, Status: w.Status, Start: w.Start, LastSeen: w.LastSeen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AllRules returns the compiled inhibit rules in tree order, de-duplicated.
func (e *Engine) AllRules() []inhibit.CompiledRule {
	var out []inhibit.CompiledRule
	seen := map[string]bool{}
	for _, n := range e.tree.Order {
		for _, r := range e.nodeRules[n.Spec.ID] {
			if !seen[r.Spec.ID] {
				seen[r.Spec.ID] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// Decide returns the suppression verdict for an alert routed to nodeID at t.
func (e *Engine) Decide(a model.Alert, nodeID string, firing []model.Alert, rules []inhibit.CompiledRule, t time.Time) Decision {
	for _, s := range e.silences {
		if s.NodeID != "" && s.NodeID != nodeID {
			continue
		}
		if SilenceActive(s, t) && routing.Matches(s.Match, a.Labels) {
			return Decision{Reason: "silence", SilenceID: s.ID}
		}
	}
	if d := inhibit.Evaluate(rules, a, firing); d.Inhibited {
		return Decision{Reason: "inhibit", RuleID: d.RuleID, Sources: d.SourceIDs}
	}
	return Decision{}
}

func (e *Engine) reconcile(t time.Time) {
	firing := e.firingSnapshot()
	rules := e.AllRules()
	wanted := map[string]map[string]bool{}
	for _, a := range firing {
		for _, m := range e.tree.Route(a.Labels, t) {
			key := m.Key.Key()
			if wanted[a.ID] == nil {
				wanted[a.ID] = map[string]bool{}
			}
			wanted[a.ID][key] = true
			g := e.groups[key]
			if g == nil {
				g = &Group{
					Node:         m.Node,
					Key:          m.Key,
					Desired:      map[string]bool{},
					Suppressed:   map[string]Decision{},
					Known:        map[string]*Known{},
					PendingSince: map[string]time.Time{},
				}
				e.groups[key] = g
			}
			g.Desired[a.ID] = true
			d := e.Decide(a, m.Node.Spec.ID, firing, rules, t)
			if d.Reason != "" {
				g.Suppressed[a.ID] = d
				if d.Reason == "silence" {
					e.everSilenced[a.ID] = true
				} else {
					e.everInhibited[a.ID] = true
				}
			} else {
				delete(g.Suppressed, a.ID)
			}
		}
	}
	for key, g := range e.groups {
		for id := range g.Desired {
			if !wanted[id][key] {
				delete(g.Desired, id)
				delete(g.Suppressed, id)
			}
		}
		e.pruneGroup(key, g)
	}
}

// pruneGroup forgets a group when nothing is firing there and every channel
// has been told the final state.
func (e *Engine) pruneGroup(key string, g *Group) {
	if len(g.Desired) > 0 {
		return
	}
	for _, k := range g.Known {
		if len(k.Alerts) > 0 {
			return
		}
	}
	delete(e.groups, key)
}

// currentChannel is the node's channel at t, honouring switches.
func (e *Engine) currentChannel(nodeID, base string, t time.Time) string {
	ch := base
	for _, s := range e.cfg.Switches {
		if s.NodeID == nodeID && !t.Before(s.At) {
			ch = s.To
		}
	}
	if ch == "" {
		ch = "default"
	}
	return ch
}

// CurrentChannel exposes channel selection for the replica layer and display.
func (e *Engine) CurrentChannel(nodeID, base string, t time.Time) string {
	return e.currentChannel(nodeID, base, t)
}

// Delta computes what channel ch of group g has not been told.
func (e *Engine) Delta(g *Group, ch string, t time.Time) model.Delta {
	k := g.Known[ch]
	var d model.Delta
	ids := make([]string, 0, len(g.Desired))
	for id := range g.Desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, sup := g.Suppressed[id]; sup {
			continue
		}
		if k != nil {
			if _, known := k.Alerts[id]; known {
				continue
			}
		}
		w := e.alerts[id]
		if w == nil || w.Status != model.StatusFiring {
			continue
		}
		d.Added = append(d.Added, model.AlertRecord{ID: id, Labels: w.Labels, Start: w.Start, Status: model.StatusFiring})
	}
	if k == nil {
		return d
	}
	kids := make([]string, 0, len(k.Alerts))
	for id := range k.Alerts {
		kids = append(kids, id)
	}
	sort.Strings(kids)
	for _, id := range kids {
		rec := k.Alerts[id]
		if g.Desired[id] {
			if _, sup := g.Suppressed[id]; !sup {
				continue // still firing and reported: no change
			}
			// Suppressed now: carry it without a resolution.
			continue
		}
		rec.Status = model.StatusResolved
		if w := e.alerts[id]; w != nil && w.Status == model.StatusResolved {
			rec.End = w.End
		} else {
			rec.End = t
		}
		d.Resolved = append(d.Resolved, rec)
	}
	return d
}

// emit generates notifications for groups whose batching wait is over.
//
// Only the group's current channel is served. When a group switches channel
// (ChannelSwitch), its state for the old channel is dropped: the old channel
// is never written to again, and the new channel starts from a baseline of
// every currently firing alert. Notifications already generated for the old
// channel but not yet delivered are abandoned by the dispatcher.
func (e *Engine) emit(t time.Time) {
	keys := make([]string, 0, len(e.groups))
	for k := range e.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := e.groups[key]
		ch := e.currentChannel(g.Node.Spec.ID, g.Node.Spec.Channel, t)
		for oldCh := range g.Known {
			if oldCh != ch {
				delete(g.Known, oldCh)
				delete(g.PendingSince, oldCh)
			}
		}
		d := e.Delta(g, ch, t)
		if len(d.Added) == 0 && len(d.Resolved) == 0 {
			delete(g.PendingSince, ch)
			continue
		}
		since, ok := g.PendingSince[ch]
		if !ok {
			since = t
			g.PendingSince[ch] = t
		}
		due := since.Add(e.cfg.GroupWait)
		if k := g.Known[ch]; k != nil {
			due = k.LastAt.Add(e.cfg.GroupInterval)
		}
		if t.Before(due) {
			continue
		}
		e.send(g, ch, d, t)
	}
}

func (e *Engine) send(g *Group, ch string, d model.Delta, t time.Time) {
	k := g.Known[ch]
	if k == nil {
		k = &Known{Alerts: map[string]model.AlertRecord{}}
		g.Known[ch] = k
	}
	n := model.Notification{
		ID:          e.nextID,
		Key:         g.KeyString(),
		NodeID:      g.Node.Spec.ID,
		LabelNames:  append([]string{}, g.Key.LabelSet...),
		GroupValues: append([]string{}, g.Key.Values...),
		Channel:     ch,
		Delta:       d,
		Suppressed:  e.suppressedNotes(g),
		LogicalAt:   t,
		Ordinal:     k.Ordinal,
		Baseline:    k.Ordinal == 0,
	}
	e.nextID++
	e.notifs = append(e.notifs, n)
	e.stats.Notifications++
	k.Ordinal++
	k.LastAt = t
	for _, a := range d.Added {
		k.Alerts[a.ID] = a
	}
	for _, a := range d.Resolved {
		delete(k.Alerts, a.ID)
	}
	delete(g.PendingSince, ch)
	e.log(t, "notify", fmt.Sprintf("%s → %s：新增 %d，解除 %d", g.Node.Spec.Name, ch, len(d.Added), len(d.Resolved)), "", g.Node.Spec.ID)
	e.timeline[len(e.timeline)-1].Key = g.KeyString()
	e.pruneGroup(g.KeyString(), g)
}

// suppressedNotes lists the group's held-back alerts, with reasons. Every
// alert in the list is firing and routed to this group but excluded from the
// delta; the reason names the silence or inhibition rule responsible.
func (e *Engine) suppressedNotes(g *Group) []model.SuppressedNote {
	ids := make([]string, 0, len(g.Suppressed))
	for id := range g.Suppressed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]model.SuppressedNote, 0, len(ids))
	for _, id := range ids {
		d := g.Suppressed[id]
		out = append(out, model.SuppressedNote{
			AlertID: id, Reason: d.Reason, RuleID: d.RuleID, SilenceID: d.SilenceID, Sources: d.Sources,
		})
	}
	return out
}

// Flush forces every group's pending delta out at t, ignoring batching waits.
// A scenario calls it at its end so the record contains the final state.
func (e *Engine) Flush(t time.Time) {
	keys := make([]string, 0, len(e.groups))
	for k := range e.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		g := e.groups[key]
		ch := e.currentChannel(g.Node.Spec.ID, g.Node.Spec.Channel, t)
		for oldCh := range g.Known {
			if oldCh != ch {
				delete(g.Known, oldCh)
			}
		}
		d := e.Delta(g, ch, t)
		if len(d.Added) == 0 && len(d.Resolved) == 0 {
			continue
		}
		e.send(g, ch, d, t)
	}
	// Give every group a chance to disappear if it is fully drained.
	for key, g := range e.groups {
		e.pruneGroup(key, g)
	}
}

// SilenceLifecycle fills ActiveStart/ActiveEnd for silences that were never
// reached by a tick before the run ended.
func (e *Engine) SilenceLifecycle(t time.Time) {
	e.markSilenceLifecycle(t)
}

// accessors

// Notifications returns logical notifications in emission order.
func (e *Engine) Notifications() []model.Notification { return e.notifs }

// Observed returns every source observation the engine was given, in order.
func (e *Engine) Observed() []model.AlertEvent { return e.observed }

// Timeline returns notable events.
func (e *Engine) Timeline() []model.TimelineEvent { return e.timeline }

// Stats returns counters.
func (e *Engine) Stats() model.RunStats { return e.stats }

// SetStats lets a caller add counters.
func (e *Engine) SetStats(f func(*model.RunStats)) { f(&e.stats) }

// Alerts returns alert instances.
func (e *Engine) Alerts() map[string]*model.Working { return e.alerts }

// AlertsSorted returns alerts ordered by start then ID.
func (e *Engine) AlertsSorted() []*model.Working {
	out := make([]*model.Working, 0, len(e.alerts))
	for _, w := range e.alerts {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start.Equal(out[j].Start) {
			return out[i].ID < out[j].ID
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out
}

// Groups returns live groups.
func (e *Engine) Groups() map[string]*Group { return e.groups }

// GroupsSorted returns live groups ordered by node then key.
func (e *Engine) GroupsSorted() []*Group {
	out := make([]*Group, 0, len(e.groups))
	for _, g := range e.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.NodeID != out[j].Key.NodeID {
			return out[i].Key.NodeID < out[j].Key.NodeID
		}
		return out[i].KeyString() < out[j].KeyString()
	})
	return out
}

// Silences returns all silences.
func (e *Engine) Silences() []*model.Silence { return e.silences }

// Tree returns the compiled tree.
func (e *Engine) Tree() *routing.Tree { return e.tree }

// Config returns the configuration.
func (e *Engine) Config() Config { return e.cfg }
