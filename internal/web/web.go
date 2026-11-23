// Package web serves the alert-flow exploration page.
//
// The page shows, for a chosen storm and a chosen instant: the routing tree
// with the alerts each node matched, the live groups, the alerts held back by
// silence or inhibition together with the reason, and the notifications that
// have been emitted. A scrubber moves through the run; the data comes from
// the engine's per-tick snapshots, so the page shows exactly the state the
// pipeline had at that instant.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"time"

	"kilnalert/internal/model"
	"kilnalert/internal/routing"
	"kilnalert/internal/sim"
	"kilnalert/internal/timewin"
)

//go:embed static/*
var staticFiles embed.FS

//go:embed index.html
var indexHTML string

var indexTemplate = template.Must(template.New("index").Parse(indexHTML))

// Server serves the page and its JSON API.
type Server struct {
	results []sim.Result
	mux     *http.ServeMux
}

// New builds a server over the loaded storm results.
func New(results []sim.Result) *Server {
	s := &Server{results: results, mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.index)
	s.mux.HandleFunc("/api/view", s.apiView)
	s.mux.Handle("/static/", http.FileServer(http.FS(staticFiles)))
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	type option struct {
		Name string
		Desc string
		OK   bool
	}
	var opts []option
	for _, res := range s.results {
		opts = append(opts, option{Name: res.Run.Name, Desc: res.Storm.Desc, OK: res.Verify.OK()})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexTemplate.Execute(w, opts)
}

// --- view model ---

type view struct {
	Storm      string          `json:"storm"`
	Desc       string          `json:"desc"`
	Seed       int64           `json:"seed"`
	Start      time.Time       `json:"start"`
	End        time.Time       `json:"end"`
	StepSecs   float64         `json:"stepSecs"`
	Now        time.Time       `json:"now"`
	Scrub      int             `json:"scrub"`
	Length     int             `json:"length"`
	Tree       []nodeView      `json:"tree"`
	Groups     []groupView     `json:"groups"`
	Silences   []silenceView   `json:"silences"`
	Notifs     []notifView     `json:"notifications"`
	Deliveries int             `json:"deliveries"`
	Stats      statsView       `json:"stats"`
	Verify     verifyView      `json:"verify"`
	Partitions []partitionView `json:"partitions"`
	Events     []eventView     `json:"events"`
	Channels   []channelView   `json:"channels"`
	Replicas   []string        `json:"replicas"`
}

type nodeView struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Path          string      `json:"path"`
	Parent        string      `json:"parent"`
	Depth         int         `json:"depth"`
	Match         string      `json:"match"`
	GroupBy       string      `json:"groupBy"`
	Channel       string      `json:"channel"`
	ChannelNow    string      `json:"channelNow"`
	Continue      bool        `json:"continueSiblings"`
	Windows       []string    `json:"windows"`
	WindowsActive bool        `json:"windowsActive"`
	Rules         []string    `json:"rules"`
	Groups        []groupView `json:"groups"`
}

type alertView struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	Text   string            `json:"text"`
	Status string            `json:"status"`
	Start  time.Time         `json:"start"`
	End    time.Time         `json:"end"`
}

type suppView struct {
	AlertID  string   `json:"alertId"`
	Text     string   `json:"text"`
	Reason   string   `json:"reason"`
	Silence  string   `json:"silenceId"`
	Rule     string   `json:"ruleId"`
	RuleText string   `json:"ruleText"`
	Sources  []string `json:"sources"`
}

type groupView struct {
	Key      string `json:"key"`
	NodeID   string `json:"nodeId"`
	NodeName string `json:"nodeName"`
	LabelSet string `json:"labelSet"`
	Values   string `json:"values"`
	Channel  string `json:"channel"`
	// Holding is how many currently firing alerts this group has not yet
	// reported to its channel (they may be new, or released from a silence
	// or inhibition, and are waiting for the batching window).
	Holding    int         `json:"holding"`
	Firing     []alertView `json:"firing"`
	Suppressed []suppView  `json:"suppressed"`
	LastSent   string      `json:"lastSent"`
	Pending    bool        `json:"pending"`
}

type notifView struct {
	ID         int         `json:"id"`
	At         time.Time   `json:"at"`
	NodeID     string      `json:"nodeId"`
	NodeName   string      `json:"nodeName"`
	Group      string      `json:"group"`
	Channel    string      `json:"channel"`
	Ordinal    int         `json:"ordinal"`
	Baseline   bool        `json:"baseline"`
	Added      []alertView `json:"added"`
	Resolved   []alertView `json:"resolved"`
	Suppressed []suppView  `json:"suppressed"`
	Delivered  bool        `json:"delivered"`
	Duplicates int         `json:"duplicates"`
	Attempts   int         `json:"attempts"`
	Replica    string      `json:"replica"`
}

type silenceView struct {
	ID     string         `json:"id"`
	Match  model.MatchAll `json:"match"`
	Text   string         `json:"text"`
	Starts time.Time      `json:"starts"`
	Ends   time.Time      `json:"ends"`
	Cancel time.Time      `json:"cancelled"`
	Reason string         `json:"reason"`
	State  string         `json:"state"`
	Active bool           `json:"active"`
	Alerts []alertView    `json:"alerts"`
}

type statsView struct {
	SourceEvents      int     `json:"sourceEvents"`
	AlertInstances    int     `json:"alertInstances"`
	Notifications     int     `json:"notifications"`
	Deliveries        int     `json:"deliveries"`
	Duplicates        int     `json:"duplicates"`
	TwiceDelivered    int     `json:"twiceDelivered"`
	NetworkFailures   int     `json:"networkFailures"`
	Retries           int     `json:"retries"`
	SuppressedSilence int     `json:"suppressedSilence"`
	SuppressedInhibit int     `json:"suppressedInhibit"`
	Compression       float64 `json:"compression"`
}

type verifyView struct {
	OK            bool     `json:"ok"`
	Expected      int      `json:"expected"`
	Actual        int      `json:"actual"`
	Matched       int      `json:"matched"`
	Problems      []string `json:"problems"`
	Delivered     int      `json:"delivered"`
	Undelivered   int      `json:"undelivered"`
	DuplicateOK   int      `json:"duplicateOk"`
	BadDuplicates int      `json:"badDuplicates"`
}

type partitionView struct {
	From   time.Time  `json:"from"`
	To     time.Time  `json:"to"`
	Groups [][]string `json:"groups"`
}

type eventView struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text"`
	AlertID string    `json:"alertId"`
	NodeID  string    `json:"nodeId"`
}

type channelView struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// --- handler ---

func (s *Server) apiView(w http.ResponseWriter, r *http.Request) {
	res, ok := s.pick(r)
	if !ok {
		http.Error(w, "没有可用的风暴结果", http.StatusNotFound)
		return
	}
	run := res.Run
	step := run.Batching.Step
	if step <= 0 {
		step = 15 * time.Second
	}
	length := int(run.End.Sub(run.Start) / time.Second)
	scrub := length
	if q := r.URL.Query().Get("t"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			scrub = n
		}
	}
	scrub = clamp(scrub, 0, length)
	now := run.Start.Add(time.Duration(scrub) * time.Second)
	snap := snapshotAt(run, now)

	tree := compileTree(run.Tree)
	var nodes []nodeView
	groupsByNode := map[string][]groupView{}
	for _, n := range tree.Order {
		nv := nodeView{
			ID: n.Spec.ID, Name: n.Spec.Name, Path: n.Path(), Parent: parentID(n), Depth: n.Depth,
			Match: routing.DescribeMatch(n.Spec.Match), GroupBy: joinNames(n.Spec.GroupBy),
			Channel: n.Spec.Channel, Continue: n.Spec.ContinueSiblings,
			Windows: windowsText(n.Spec.TimeWindows), WindowsActive: timewin.Active(n.Spec.TimeWindows, snap.At),
			Rules: ruleTexts(n.Spec.InhibitRules),
		}
		nodes = append(nodes, nv)
	}

	alerts := alertsAt(run, snap)
	alertByID := map[string]model.Alert{}
	for _, a := range alerts {
		alertByID[a.ID] = a
	}

	var groups []groupView
	for _, g := range snap.Groups {
		gv := groupView{
			Key: g.NodeID + "|" + joinNames(g.Values), NodeID: g.NodeID,
			NodeName: nodeName(tree, g.NodeID), LabelSet: joinNames(g.LabelSet),
			Values: joinNames(g.Values), Channel: g.Channel, Pending: g.Pending,
		}
		if !g.LastSent.IsZero() {
			gv.LastSent = g.LastSent.In(run.Start.Location()).Format("15:04:05")
		}
		for _, id := range g.Firing {
			if a, ok := alertByID[id]; ok {
				gv.Firing = append(gv.Firing, alertOf(a))
			}
		}
		// Holding is derived from the notification record: any alert this
		// group has not yet reported to its channel is waiting.
		known := reportedCount(run, g.NodeID, g.Values, g.Channel, snap.At)
		if len(g.Firing) > known {
			gv.Holding = len(g.Firing) - known
		}
		for _, sp := range g.Suppressed {
			sv := suppView{
				AlertID: sp.AlertID, Reason: reasonText(sp.Reason), Silence: sp.SilenceID,
				Rule: sp.RuleID, Sources: sp.Sources, RuleText: ruleDescription(run.Tree, sp.RuleID),
			}
			if a, ok := alertByID[sp.AlertID]; ok {
				sv.Text = model.String(a.Labels)
			}
			gv.Suppressed = append(gv.Suppressed, sv)
		}
		if gv.Firing == nil {
			gv.Firing = []alertView{}
		}
		if gv.Suppressed == nil {
			gv.Suppressed = []suppView{}
		}
		sortAlertViews(gv.Firing)
		sortSuppViews(gv.Suppressed)
		groups = append(groups, gv)
		groupsByNode[g.NodeID] = append(groupsByNode[g.NodeID], gv)
	}
	for i := range nodes {
		nodes[i].Groups = groupsByNode[nodes[i].ID]
		if nodes[i].Groups == nil {
			nodes[i].Groups = []groupView{}
		}
		if nodes[i].Windows == nil {
			nodes[i].Windows = []string{}
		}
		if nodes[i].Rules == nil {
			nodes[i].Rules = []string{}
		}
		base := ""
		if n := tree.ByID[nodes[i].ID]; n != nil {
			base = n.Spec.Channel
		}
		nodes[i].ChannelNow = channelNow(run, nodes[i].ID, base, snap.At)
		if nodes[i].ChannelNow == "" {
			nodes[i].ChannelNow = "默认"
		}
		if nodes[i].Channel == "" {
			nodes[i].Channel = "默认"
		}
	}

	if groups == nil {
		groups = []groupView{}
	}
	v := view{
		Storm: run.Name, Desc: res.Storm.Desc, Seed: run.Seed, Start: run.Start, End: run.End,
		StepSecs: step.Seconds(), Now: snap.At, Scrub: scrub, Length: length,
		Tree: nodes, Groups: groups, Replicas: run.Replicas,
		Stats:      liveStats(run, snap.At, res.Verify.CompressionRatio),
		Verify:     verifyOf(res),
		Deliveries: countDeliveries(run, snap.At),
		Silences:   []silenceView{},
		Notifs:     []notifView{},
		Events:     []eventView{},
		Partitions: []partitionView{},
		Channels:   []channelView{},
	}

	// Notifications emitted up to the snapshot.
	delivByNotif := map[int][]model.Delivery{}
	for _, d := range run.Deliveries {
		delivByNotif[d.NotificationID] = append(delivByNotif[d.NotificationID], d)
	}
	for _, n := range run.Notifications {
		if n.LogicalAt.After(snap.At) {
			continue
		}
		nv := notifView{
			ID: n.ID, At: n.LogicalAt, NodeID: n.NodeID, NodeName: nodeName(tree, n.NodeID),
			Group: joinNames(n.GroupValues), Channel: n.Channel, Ordinal: n.Ordinal, Baseline: n.Baseline,
			Added: recordsOf(n.Delta.Added), Resolved: recordsOf(n.Delta.Resolved),
		}
		for _, sp := range n.Suppressed {
			sv := suppView{AlertID: sp.AlertID, Reason: reasonText(sp.Reason), Silence: sp.SilenceID, Rule: sp.RuleID, Sources: sp.Sources, RuleText: ruleDescription(run.Tree, sp.RuleID)}
			if a, ok := alertByID[sp.AlertID]; ok {
				sv.Text = model.String(a.Labels)
			}
			nv.Suppressed = append(nv.Suppressed, sv)
		}
		if nv.Suppressed == nil {
			nv.Suppressed = []suppView{}
		}
		sortSuppViews(nv.Suppressed)
		for _, d := range delivByNotif[n.ID] {
			if d.At.After(snap.At) {
				continue
			}
			nv.Attempts++
			if d.OK {
				nv.Delivered = true
				if nv.Replica == "" {
					nv.Replica = d.Replica
				}
			}
			if d.Duplicate {
				nv.Duplicates++
			}
		}
		v.Notifs = append(v.Notifs, nv)
	}

	// Silences as of the snapshot.
	for i, sv := range run.Silences {
		sil := sv.Silence
		active := snap.SilenceActive != nil && i < len(snap.SilenceActive) && snap.SilenceActive[i]
		view := silenceView{
			ID: sil.ID, Match: sil.Match, Text: routing.DescribeMatch(sil.Match),
			Starts: sil.Starts, Ends: sil.Ends, Cancel: sil.CancelledAt,
			Reason: sil.Reason, State: silenceState(sil, snap.At), Active: active,
		}
		if active {
			for _, a := range alerts {
				if a.Status == model.StatusFiring && routing.Matches(sil.Match, a.Labels) {
					view.Alerts = append(view.Alerts, alertOf(a))
				}
			}
			sortAlertViews(view.Alerts)
		}
		v.Silences = append(v.Silences, view)
	}

	for _, p := range run.Partitions {
		if p.From.After(snap.At) {
			continue
		}
		v.Partitions = append(v.Partitions, partitionView{From: p.From, To: p.To, Groups: p.Groups})
	}
	for _, e := range run.Events {
		if e.At.After(snap.At) {
			continue
		}
		v.Events = append(v.Events, eventView{At: e.At, Kind: e.Kind, Text: e.Text, AlertID: e.AlertID, NodeID: e.NodeID})
	}
	for _, c := range run.Channels {
		v.Channels = append(v.Channels, channelView{Name: c, Desc: channelDesc(c)})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (s *Server) pick(r *http.Request) (sim.Result, bool) {
	name := r.URL.Query().Get("storm")
	for _, res := range s.results {
		if res.Run.Name == name {
			return res, true
		}
	}
	if len(s.results) == 0 {
		return sim.Result{}, false
	}
	return s.results[0], true
}

// --- helpers ---

func compileTree(spec *model.RouteNode) *routing.Tree {
	t, err := routing.Compile(spec)
	if err != nil {
		return &routing.Tree{ByID: map[string]*routing.Node{}}
	}
	return t
}

func snapshotAt(run model.RunResult, t time.Time) model.Snapshot {
	var snap model.Snapshot
	for _, s := range run.Snapshots {
		if s.At.After(t) {
			break
		}
		snap = s
	}
	if snap.At.IsZero() {
		snap.At = run.Start
	}
	return snap
}

// alertsAt returns the alerts in the snapshot, or — if snapshots are absent —
// those derivable from the run's final alert records.
func alertsAt(run model.RunResult, snap model.Snapshot) []model.Alert {
	if len(snap.Alerts) > 0 || !snap.At.IsZero() && snap.At.Equal(run.Start) {
		return snap.Alerts
	}
	return nil
}

func parentID(n *routing.Node) string {
	if n.Parent == nil {
		return ""
	}
	return n.Parent.Spec.ID
}

func nodeName(tree *routing.Tree, id string) string {
	if n := tree.ByID[id]; n != nil {
		return n.Path()
	}
	return id
}

func alertOf(a model.Alert) alertView {
	return alertView{ID: a.ID, Labels: a.Labels, Text: model.String(a.Labels), Status: string(a.Status), Start: a.Start, End: a.End}
}

func recordsOf(recs []model.AlertRecord) []alertView {
	out := make([]alertView, 0, len(recs))
	for _, r := range recs {
		out = append(out, alertView{ID: r.ID, Labels: r.Labels, Text: model.String(r.Labels), Status: string(r.Status), Start: r.Start, End: r.End})
	}
	return out
}

func reasonText(r string) string {
	if r == "silence" {
		return "静默"
	}
	if r == "inhibit" {
		return "抑制"
	}
	return r
}

func ruleDescription(spec *model.RouteNode, id string) string {
	if id == "" {
		return ""
	}
	for _, r := range allRules(spec) {
		if r.ID == id {
			return routing.DescribeMatch(r.Source) + " → " + routing.DescribeMatch(r.Target) + "，同 " + joinNames(r.Equal)
		}
	}
	return id
}

func allRules(n *model.RouteNode) []model.InhibitRule {
	out := append([]model.InhibitRule{}, n.InhibitRules...)
	for _, c := range n.Children {
		out = append(out, allRules(c)...)
	}
	return out
}

func ruleTexts(rs []model.InhibitRule) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, routing.DescribeMatch(r.Source)+" → "+routing.DescribeMatch(r.Target)+"（同 "+joinNames(r.Equal)+"）")
	}
	return out
}

func windowsText(ws []model.TimeWindow) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, timewin.Describe(w))
	}
	return out
}

// reportedCount returns how many alerts of a group the channel was told are
// firing, as of t, by replaying the group's notifications to that channel.
func reportedCount(run model.RunResult, nodeID string, values []string, channel string, t time.Time) int {
	known := map[string]bool{}
	for _, n := range run.Notifications {
		if n.NodeID != nodeID || n.Channel != channel || n.LogicalAt.After(t) {
			continue
		}
		if joinNames(n.GroupValues) != joinNames(values) {
			continue
		}
		for _, a := range n.Delta.Added {
			known[a.ID] = true
		}
		for _, a := range n.Delta.Resolved {
			delete(known, a.ID)
		}
	}
	return len(known)
}

func channelNow(run model.RunResult, nodeID, base string, t time.Time) string {
	ch := base
	for _, s := range run.Switches {
		if s.NodeID == nodeID && !t.Before(s.At) {
			ch = s.To
		}
	}
	return ch
}

func silenceState(s model.Silence, t time.Time) string {
	if !s.CancelledAt.IsZero() && !s.CancelledAt.After(t) {
		if s.CancelledAt.Before(s.Starts) {
			return "生效前已取消"
		}
		return "已提前取消"
	}
	if !t.Before(s.Ends) {
		return "已到期"
	}
	if t.Before(s.Starts) {
		return "未生效"
	}
	return "生效中"
}

func verifyOf(res sim.Result) verifyView {
	return verifyView{
		OK: res.Verify.OK(), Expected: res.Verify.Expected, Actual: res.Verify.Actual,
		Matched: res.Verify.Matched, Problems: res.Verify.Problems(),
		Delivered: res.Verify.Delivered, Undelivered: len(res.Verify.Undelivered),
		DuplicateOK: res.Verify.DuplicateDeliveries, BadDuplicates: len(res.Verify.DuplicatesOutsidePartition),
	}
}

// liveStats summarises the run as of instant t: notification, delivery and
// retry counts stop at the cursor, so scrubbing back shows fewer.
func liveStats(run model.RunResult, t time.Time, compression float64) statsView {
	s := statsView{SourceEvents: run.Stats.SourceEvents, AlertInstances: run.Stats.AlertInstances,
		Compression: compression}
	for _, n := range run.Notifications {
		if !n.LogicalAt.After(t) {
			s.Notifications++
		}
	}
	dupBy := map[string]map[string]bool{}
	for _, d := range run.Deliveries {
		if d.At.After(t) {
			continue
		}
		if d.Attempt > 0 {
			s.Retries++
		}
		if !d.OK {
			s.NetworkFailures++
			continue
		}
		s.Deliveries++
		if d.Duplicate {
			s.Duplicates++
		}
		if dupBy[d.DedupeID] == nil {
			dupBy[d.DedupeID] = map[string]bool{}
		}
		dupBy[d.DedupeID][d.Replica] = true
	}
	for _, reps := range dupBy {
		if len(reps) > 1 {
			s.TwiceDelivered++
		}
	}
	// Suppression totals as of t: each alert held back at some tick is
	// counted once, by the reason that first held it.
	silenced := map[string]bool{}
	inhibited := map[string]bool{}
	for _, sn := range run.Snapshots {
		if sn.At.After(t) {
			break
		}
		for _, g := range sn.Groups {
			for _, sp := range g.Suppressed {
				if sp.Reason == "silence" {
					silenced[sp.AlertID] = true
				} else {
					inhibited[sp.AlertID] = true
				}
			}
		}
	}
	for id := range silenced {
		if !inhibited[id] {
			s.SuppressedSilence++
		}
	}
	s.SuppressedInhibit = len(inhibited)
	return s
}

func countDeliveries(run model.RunResult, t time.Time) int {
	n := 0
	for _, d := range run.Deliveries {
		if d.OK && !d.At.After(t) {
			n++
		}
	}
	return n
}

func channelDesc(name string) string {
	for _, c := range sim.Channels() {
		if c.Name == name {
			return sim.DescribeChannel(c)
		}
	}
	return ""
}

func joinNames(xs []string) string {
	if len(xs) == 0 {
		return "（全部）"
	}
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += " / "
		}
		s += x
	}
	return s
}

func sortAlertViews(xs []alertView) {
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].Text < xs[j].Text })
}

func sortSuppViews(xs []suppView) {
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].Text < xs[j].Text })
}

func clamp(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// Serve starts the page on addr and blocks.
func Serve(addr string, results []sim.Result) error {
	srv := &http.Server{Addr: addr, Handler: New(results), ReadHeaderTimeout: 5 * time.Second}
	fmt.Println("页面已启动： http://" + addr + "/")
	return srv.ListenAndServe()
}
