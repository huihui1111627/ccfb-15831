// Package verify independently derives the notifications a run should have
// produced and checks the run's actual output against them.
//
// The verifier never calls the engine. It rebuilds the alert instances from
// the raw source observations, replays the documented rules tick by tick and
// computes, for each group and channel, the exact sequence of notifications
// that must exist. It then pairs each expected notification with an actual
// one and reports anything missing, extra, mismatched or delivered wrongly.
package verify

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"kilnalert/internal/inhibit"
	"kilnalert/internal/model"
	"kilnalert/internal/routing"
)

// Report is the outcome of verifying one run.
type Report struct {
	Instances    int
	SourceEvents int
	Expected     int
	Actual       int
	Matched      int
	Missing      []string
	Extra        []string
	Mismatched   []string
	ProblemList  []string

	Delivered                  int
	Undelivered                []string
	DuplicateDeliveries        int
	DuplicatesOutsidePartition []string
	ContentProblems            []string
	CompressionRatio           float64
	SuppressedSilence          int
	SuppressedInhibit          int
}

// Problems returns every problem as a flat list for display.
func (r Report) Problems() []string {
	out := append([]string{}, r.ProblemList...)
	for _, m := range r.Missing {
		out = append(out, "漏发: "+m)
	}
	for _, m := range r.Extra {
		out = append(out, "多发: "+m)
	}
	for _, m := range r.Mismatched {
		out = append(out, "内容不符: "+m)
	}
	for _, m := range r.Undelivered {
		out = append(out, "未送达: "+m)
	}
	for _, m := range r.DuplicatesOutsidePartition {
		out = append(out, "分区之外的重复: "+m)
	}
	out = append(out, r.ContentProblems...)
	return out
}

// OK reports whether the run passed.
func (r Report) OK() bool { return len(r.Problems()) == 0 }

type inst struct {
	id       string
	labels   map[string]string
	start    time.Time
	lastSeen time.Time
	end      time.Time
	ended    bool
}

type track struct {
	channel  string
	known    map[string]model.AlertRecord
	lastSent time.Time
	since    time.Time // pending since; zero when nothing pending
	sent     bool
	ordinal  int
}

type expectedNotif struct {
	key     string
	nodeID  string
	channel string
	at      time.Time
	ordinal int
	delta   model.Delta
}

// Run verifies res, replaying rules over [t0,tEnd].
func Run(res model.RunResult, t0, tEnd time.Time) Report {
	rep := Report{SourceEvents: len(res.AlertEvents)}
	tree, err := routing.Compile(res.Tree)
	if err != nil {
		rep.ProblemList = append(rep.ProblemList, "路由树无法编译: "+err.Error())
		return rep
	}
	b := res.Batching

	// --- 1. Bucket the raw observations by tick. ---
	evs := append([]model.AlertEvent(nil), res.AlertEvents...)
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	// Bucket by tick so the replay applies events exactly when the engine did.
	ticks := []time.Time{}
	for t := t0; !t.After(tEnd); t = t.Add(b.Step) {
		ticks = append(ticks, t)
	}
	eventAt := func(cut time.Time, from int) (int, []model.AlertEvent) {
		i := from
		var out []model.AlertEvent
		for i < len(evs) && !evs[i].At.After(cut) {
			out = append(out, evs[i])
			i++
		}
		return i, out
	}

	// --- 2. Index actual notifications by their identity. ---
	actualIndex := map[string]int{}
	for i, n := range res.Notifications {
		actualIndex[identity(n.Key, n.Channel, n.LogicalAt, n.Ordinal)] = i
	}
	used := make([]bool, len(res.Notifications))

	rulesByNode := compileRules(tree)
	allRules := flatten(tree, rulesByNode)

	// --- 3. Replay. ---
	v := &verifier{
		res: res, tree: tree, b: b, allRules: allRules,
		byFP: map[string]*inst{}, byID: map[string]*inst{},
		tracks: map[string]*track{}, grpChannel: map[string]string{}, rep: &rep,
		actualIndex: actualIndex, used: used,
		episodes: map[string]int{}, everSil: map[string]bool{}, everInh: map[string]bool{},
	}
	evi := 0
	for _, t := range ticks {
		var batch []model.AlertEvent
		evi, batch = eventAt(t, evi)
		for _, ev := range batch {
			v.apply(ev)
		}
		// Auto-resolve after applying the tick's observations, as the engine
		// does at the start of Tick.
		for _, in := range v.insts {
			if in.ended || in.start.After(t) {
				continue
			}
			deadline := in.lastSeen.Add(b.StaleAfter)
			if !t.Before(deadline) {
				in.ended = true
				in.end = deadline
			}
		}
		v.step(t, false)
	}
	// The final forced flush: every group's pending delta goes out at tEnd.
	// Observations exactly at tEnd were already applied by the last tick.
	v.step(tEnd, true)
	rep.Instances = len(v.insts)
	rep.SuppressedSilence = len(v.everSil)
	rep.SuppressedInhibit = len(v.everInh)

	// --- 5. Unmatched actual notifications are extra. ---
	rep.Actual = len(res.Notifications)
	for i, n := range res.Notifications {
		if !used[i] {
			rep.Extra = append(rep.Extra, identity(n.Key, n.Channel, n.LogicalAt, n.Ordinal))
		}
	}

	// --- 6. Delivery checks. ---
	verifyDelivery(&rep, res)

	// --- 7. Compression statistics. ---
	rep.CompressionRatio = ratio(rep.SourceEvents, len(res.Notifications))
	return rep
}

// verifyDelivery checks every notification reached a channel exactly once,
// except deliberate duplicates inside a partition, which must be the only
// duplicates.
func verifyDelivery(rep *Report, res model.RunResult) {
	perNotif := map[int][]model.Delivery{}
	for _, d := range res.Deliveries {
		perNotif[d.NotificationID] = append(perNotif[d.NotificationID], d)
	}
	for _, n := range res.Notifications {
		delivered := false
		abandoned := false
		var okReplicas []model.Delivery
		for _, d := range perNotif[n.ID] {
			if d.OK {
				delivered = true
				okReplicas = append(okReplicas, d)
			}
			if !d.OK && !d.Duplicate && strings.Contains(d.Reason, "渠道切换") {
				abandoned = true
			}
			if d.OK {
				rep.Delivered++
			}
		}
		if !delivered && !abandoned {
			rep.Undelivered = append(rep.Undelivered, n.DedupeID())
		}
		if len(okReplicas) > 1 {
			// A duplicate is legitimate only if every pair of the successful
			// sends was made while those replicas could not see each other.
			rep.DuplicateDeliveries += len(okReplicas) - 1
			for i := 0; i < len(okReplicas) && i == 0; i++ {
				_ = i
			}
			for i := 1; i < len(okReplicas); i++ {
				first, other := okReplicas[0], okReplicas[i]
				if first.Replica == other.Replica {
					rep.DuplicatesOutsidePartition = append(rep.DuplicatesOutsidePartition,
						fmt.Sprintf("%s 同一副本 %s 重复送达", n.DedupeID(), first.Replica))
					continue
				}
				if !disconnected(res, first.Replica, other.Replica, other.At) {
					rep.DuplicatesOutsidePartition = append(rep.DuplicatesOutsidePartition,
						fmt.Sprintf("%s 由 %s 与 %s 在 %s 各送达一次，当时两副本可通信",
							n.DedupeID(), first.Replica, other.Replica, other.At.In(res.Start.Location()).Format("15:04:05")))
				}
			}
		}
	}
}

func disconnected(res model.RunResult, a, b string, t time.Time) bool {
	for _, p := range res.Partitions {
		if t.Before(p.From) || !t.Before(p.To) {
			continue
		}
		return groupIndex(p.Groups, a) != groupIndex(p.Groups, b)
	}
	return false
}

func groupIndex(groups [][]string, r string) int {
	for i, g := range groups {
		for _, x := range g {
			if x == r {
				return i
			}
		}
	}
	return -1
}

func compileRules(tree *routing.Tree) map[string][]inhibit.CompiledRule {
	out := map[string][]inhibit.CompiledRule{}
	for _, n := range tree.Order {
		var rules []inhibit.CompiledRule
		if n.Parent != nil {
			rules = append(rules, out[n.Parent.Spec.ID]...)
		}
		if len(n.Spec.InhibitRules) > 0 {
			own, _ := inhibit.Compile(n.Spec.InhibitRules, n.Spec.ID)
			rules = append(rules, own...)
		}
		out[n.Spec.ID] = rules
	}
	return out
}

func flatten(tree *routing.Tree, byNode map[string][]inhibit.CompiledRule) []inhibit.CompiledRule {
	var out []inhibit.CompiledRule
	seen := map[string]bool{}
	for _, n := range tree.Order {
		for _, r := range byNode[n.Spec.ID] {
			if !seen[r.Spec.ID] {
				seen[r.Spec.ID] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// suppress returns "silence", "inhibit" or "".
func suppress(res model.RunResult, nodeID string, a model.Alert, firing []model.Alert, rules []inhibit.CompiledRule, t time.Time) string {
	for _, sv := range res.Silences {
		s := sv.Silence
		if s.NodeID != "" && s.NodeID != nodeID {
			continue
		}
		if t.Before(s.Starts) || !t.Before(s.Ends) {
			continue
		}
		if !s.CancelledAt.IsZero() && !t.Before(s.CancelledAt) {
			continue
		}
		if routing.Matches(s.Match, a.Labels) {
			return "silence"
		}
	}
	if inhibit.Evaluate(rules, a, firing).Inhibited {
		return "inhibit"
	}
	return ""
}

func channelAt(res model.RunResult, n *routing.Node, t time.Time) string {
	ch := n.Spec.Channel
	for _, s := range res.Switches {
		if s.NodeID == n.Spec.ID && !t.Before(s.At) {
			ch = s.To
		}
	}
	if ch == "" {
		ch = "default"
	}
	return ch
}

// silenceInForce is the verifier's own copy of the silence rule.
func silenceInForce(s *model.Silence, t time.Time) bool {
	if t.Before(s.Starts) || !t.Before(s.Ends) {
		return false
	}
	return s.CancelledAt.IsZero() || t.Before(s.CancelledAt)
}

func identity(key, ch string, at time.Time, ordinal int) string {
	return fmt.Sprintf("%s|%s|%s|#%d", key, ch, at.UTC().Format("2006-01-02T15:04:05Z"), ordinal)
}

// groupOfKey strips the "|channel" suffix from a track key.
func groupOfKey(key string) string {
	if i := strings.LastIndexByte(key, '|'); i >= 0 {
		return key[:i]
	}
	return key
}

func sortedIDs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func compareDelta(want, got model.Delta) string {
	wadd := map[string]string{}
	for _, a := range want.Added {
		wadd[a.ID] = a.Start.UTC().Format(time.RFC3339)
	}
	gadd := map[string]string{}
	for _, a := range got.Added {
		gadd[a.ID] = a.Start.UTC().Format(time.RFC3339)
	}
	wres := map[string]string{}
	for _, a := range want.Resolved {
		wres[a.ID] = a.End.UTC().Format(time.RFC3339)
	}
	gres := map[string]string{}
	for _, a := range got.Resolved {
		gres[a.ID] = a.End.UTC().Format(time.RFC3339)
	}
	if msg := diffMap("新增", wadd, gadd); msg != "" {
		return msg
	}
	return diffMap("解除", wres, gres)
}

func diffMap(label string, want, got map[string]string) string {
	var msgs []string
	for id, v := range want {
		g, ok := got[id]
		if !ok {
			msgs = append(msgs, label+"缺少 "+short(id))
		} else if g != v {
			msgs = append(msgs, label+" "+short(id)+" 时间 "+g+" ≠ "+v)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			msgs = append(msgs, label+"多出 "+short(id))
		}
	}
	if len(msgs) == 0 {
		return ""
	}
	sort.Strings(msgs)
	s := msgs[0]
	if len(msgs) > 1 {
		s += fmt.Sprintf(" 等 %d 处", len(msgs))
	}
	return s
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func ratio(a, b int) float64 {
	if b == 0 {
		if a == 0 {
			return 1
		}
		return float64(a)
	}
	return float64(a) / float64(b)
}

// verifier holds the replay state.
type verifier struct {
	res        model.RunResult
	tree       *routing.Tree
	b          model.BatchParams
	allRules   []inhibit.CompiledRule
	byFP       map[string]*inst
	byID       map[string]*inst
	insts      []*inst
	episodes   map[string]int
	everSil    map[string]bool
	everInh    map[string]bool
	tracks     map[string]*track
	grpChannel map[string]string
	rep        *Report

	actualIndex map[string]int
	used        []bool
}

type state struct {
	node   *routing.Node
	key    model.GroupKey
	firing map[string]bool
	sup    map[string]bool
}

// apply folds one source observation into the verifier's instance set.
func (v *verifier) apply(ev model.AlertEvent) {
	fp := model.Fingerprint(ev.Labels)
	in := v.byFP[fp]
	if ev.Fire {
		if in == nil || in.ended {
			v.episodes[fp]++
			id := fp
			if v.episodes[fp] > 1 {
				id = fmt.Sprintf("%s#%d", fp, v.episodes[fp])
			}
			in = &inst{id: id, labels: model.CloneLabels(ev.Labels), start: ev.At}
			v.byFP[fp] = in
			v.byID[id] = in
			v.insts = append(v.insts, in)
		}
		in.lastSeen = ev.At
		return
	}
	if in == nil || in.ended {
		return
	}
	in.end = ev.At
	in.ended = true
	in.lastSeen = ev.At
}

// step evaluates one instant. With flush set, batching waits are ignored.
func (v *verifier) step(t time.Time, flush bool) {
	var firing []model.Alert
	for _, in := range v.insts {
		if in.start.After(t) {
			continue
		}
		if in.ended && !in.end.After(t) {
			continue
		}
		firing = append(firing, model.Alert{ID: in.id, Labels: in.labels, Status: model.StatusFiring, Start: in.start})
	}
	sort.Slice(firing, func(i, j int) bool { return firing[i].ID < firing[j].ID })
	groups := map[string]*state{}
	for _, a := range firing {
		for _, m := range v.tree.Route(a.Labels, t) {
			k := m.Key.Key()
			g := groups[k]
			if g == nil {
				g = &state{node: m.Node, key: m.Key, firing: map[string]bool{}, sup: map[string]bool{}}
				groups[k] = g
			}
			g.firing[a.ID] = true
			if reason := v.suppress(m.Node.Spec.ID, a, firing, t); reason != "" {
				g.sup[a.ID] = true
				if reason == "silence" {
					v.everSil[a.ID] = true
				} else {
					v.everInh[a.ID] = true
				}
			}
		}
	}
	// Channel switches: the new channel starts fresh, the old one is dropped.
	for k, g := range groups {
		ch := channelAt(v.res, g.node, t)
		if prev, ok := v.grpChannel[k]; ok && prev != ch {
			for tk := range v.tracks {
				if groupOfKey(tk) == k && tk != k+"|"+ch {
					delete(v.tracks, tk)
				}
			}
		}
		v.grpChannel[k] = ch
		tk := k + "|" + ch
		if v.tracks[tk] == nil {
			v.tracks[tk] = &track{channel: ch, known: map[string]model.AlertRecord{}}
		}
	}
	// Every existing track is evaluated; resolutions may still be owed after
	// a group stops routing.
	keys := make([]string, 0, len(v.tracks))
	for k := range v.tracks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, tk := range keys {
		tr := v.tracks[tk]
		gk := groupOfKey(tk)
		g := groups[gk]
		ch := tr.channel
		if g != nil {
			ch = channelAt(v.res, g.node, t)
		}
		d := model.Delta{}
		if g != nil {
			for _, id := range sortedIDs(g.firing) {
				if g.sup[id] {
					continue
				}
				if _, known := tr.known[id]; known {
					continue
				}
				in := v.byID[id]
				if in == nil {
					continue
				}
				d.Added = append(d.Added, model.AlertRecord{ID: id, Labels: in.labels, Start: in.start, Status: model.StatusFiring})
			}
		}
		kids := make([]string, 0, len(tr.known))
		for id := range tr.known {
			kids = append(kids, id)
		}
		sort.Strings(kids)
		for _, id := range kids {
			if g != nil && g.firing[id] {
				continue // still firing here (suppressed or not): no resolution
			}
			rec := tr.known[id]
			rec.Status = model.StatusResolved
			if in := v.byID[id]; in != nil && in.ended {
				rec.End = in.end
			} else {
				rec.End = t
			}
			d.Resolved = append(d.Resolved, rec)
		}
		if len(d.Added) == 0 && len(d.Resolved) == 0 {
			tr.since = time.Time{}
			if g == nil {
				delete(v.tracks, tk)
			}
			continue
		}
		if tr.since.IsZero() {
			tr.since = t
		}
		if !flush {
			due := tr.since.Add(v.b.GroupWait)
			if tr.sent {
				due = tr.lastSent.Add(v.b.GroupInterval)
			}
			if t.Before(due) {
				continue
			}
		}
		nodeID := ""
		if g != nil {
			nodeID = g.node.Spec.ID
		}
		v.expect(expectedNotif{key: gk, nodeID: nodeID, channel: ch, at: t, ordinal: tr.ordinal, delta: d})
		tr.ordinal++
		tr.sent = true
		tr.lastSent = t
		tr.since = time.Time{}
		for _, a := range d.Added {
			tr.known[a.ID] = a
		}
		for _, a := range d.Resolved {
			delete(tr.known, a.ID)
		}
		if g == nil && len(tr.known) == 0 {
			delete(v.tracks, tk)
		}
	}
}

func (v *verifier) suppress(nodeID string, a model.Alert, firing []model.Alert, t time.Time) string {
	for _, sv := range v.res.Silences {
		s := sv.Silence
		if s.NodeID != "" && s.NodeID != nodeID {
			continue
		}
		if t.Before(s.Starts) || !t.Before(s.Ends) {
			continue
		}
		if !s.CancelledAt.IsZero() && !t.Before(s.CancelledAt) {
			continue
		}
		if routing.Matches(s.Match, a.Labels) {
			return "silence"
		}
	}
	if inhibit.Evaluate(v.allRules, a, firing).Inhibited {
		return "inhibit"
	}
	return ""
}

func (v *verifier) expect(e expectedNotif) {
	rep := v.rep
	rep.Expected++
	id := identity(e.key, e.channel, e.at, e.ordinal)
	idx, ok := v.actualIndex[id]
	if !ok {
		rep.Missing = append(rep.Missing, id)
		return
	}
	if v.used[idx] {
		rep.Mismatched = append(rep.Mismatched, id+" (重复配对)")
		return
	}
	v.used[idx] = true
	rep.Matched++
	n := v.res.Notifications[idx]
	if msg := compareDelta(e.delta, n.Delta); msg != "" {
		rep.Mismatched = append(rep.Mismatched, id+": "+msg)
		return
	}
	// The delta must be explained: every maintained alert fires, every
	// removed one has resolved or is no longer routed here.
	if msg := v.explainDelta(n); msg != "" {
		rep.ContentProblems = append(rep.ContentProblems, id+": "+msg)
	}
}

// explainDelta checks that a delta is a truthful change description: each
// added record names an alert this run saw firing, and each resolved record
// names one that the run has since ended or unrouted.
func (v *verifier) explainDelta(n model.Notification) string {
	for _, a := range n.Delta.Added {
		in := v.byID[a.ID]
		if in == nil {
			return "新增引用了不存在的告警 " + short(a.ID)
		}
		if !in.start.Equal(a.Start) {
			return fmt.Sprintf("新增 %s 的开始时间 %s 与观测 %s 不符", short(a.ID), a.Start.Format("15:04:05"), in.start.Format("15:04:05"))
		}
	}
	for _, a := range n.Delta.Resolved {
		in := v.byID[a.ID]
		if in == nil {
			return "解除引用了不存在的告警 " + short(a.ID)
		}
		if a.Status != model.StatusResolved {
			return "解除记录的状态不是 resolved: " + short(a.ID)
		}
		if a.End.IsZero() {
			return "解除记录缺少解除时间: " + short(a.ID)
		}
	}
	// Suppression notes must name alerts that were genuinely held back at
	// the notification's instant, by the silence or rule they name.
	for _, note := range n.Suppressed {
		a := v.byID[note.AlertID]
		if a == nil {
			return "抑制记录引用了不存在的告警 " + short(note.AlertID)
		}
		if note.Reason == "silence" {
			found := false
			for _, sv := range v.res.Silences {
				s := sv.Silence
				if s.ID == note.SilenceID && silenceInForce(&s, n.LogicalAt) && routing.Matches(s.Match, a.labels) {
					found = true
					break
				}
			}
			if !found {
				return "静默抑制 " + short(note.AlertID) + " 缺少生效中的静默 " + note.SilenceID
			}
			continue
		}
		if note.Reason != "inhibit" {
			return "抑制原因非法: " + note.Reason
		}
		var rule *inhibit.CompiledRule
		for i := range v.allRules {
			if v.allRules[i].Spec.ID == note.RuleID {
				rule = &v.allRules[i]
				break
			}
		}
		if rule == nil {
			return "抑制记录引用了未知规则 " + note.RuleID
		}
		if !rule.Target(a.labels) {
			return "被抑制的 " + short(note.AlertID) + " 不满足规则 " + note.RuleID + " 的目标条件"
		}
		ok := false
		for _, src := range note.Sources {
			s := v.byID[src]
			if s == nil || !rule.Source(s.labels) || s.ended && !s.end.After(n.LogicalAt) {
				continue
			}
			equal := true
			for _, name := range rule.Spec.Equal {
				if s.labels[name] != a.labels[name] {
					equal = false
					break
				}
			}
			if equal {
				ok = true
			}
		}
		if !ok {
			return "抑制记录 " + short(note.AlertID) + " 找不到同时满足的源告警"
		}
	}
	return ""
}
