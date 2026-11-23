// Package sim generates reproducible alert storms, runs them through the
// engine and verifies the result.
package sim

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"kilnalert/internal/channel"
	"kilnalert/internal/engine"
	"kilnalert/internal/model"
	"kilnalert/internal/routing"
	"kilnalert/internal/verify"
)

// Replicas are the notification-service replicas, in send order.
var Replicas = []string{"kln-0", "kln-1", "kln-2"}

// Channels are the simulated notification channels.
func Channels() []channel.Config {
	return channels
}

// ChannelsFor returns the channels of a scenario. A scenario may declare a
// channel outage, which injects failures on its first attempts.
func ChannelsFor(cfg Config) []channel.Config {
	out := append([]channel.Config(nil), channels...)
	for i := range out {
		if cfg.ChannelOutage && out[i].Name == "短信" {
			out[i].FailRounds = 5
			out[i].FailEvery = 3
		}
	}
	return out
}

var channels = []channel.Config{
	{Name: "看板", Stagger: 15 * time.Second, ReplicaGap: 5 * time.Second},
	{Name: "短信", Stagger: 30 * time.Second, ReplicaGap: 8 * time.Second, FailEvery: 4},
	{Name: "邮件", Stagger: 1 * time.Minute, ReplicaGap: 20 * time.Second, FailRounds: 2, FailEvery: 7},
	{Name: "电话", Stagger: 2 * time.Minute, ReplicaGap: 30 * time.Second},
}

// Backoff is the retry policy.
var Backoff = channel.Backoff{Base: 30 * time.Second, Max: 8 * time.Minute}

// Storm describes one scenario.
type Storm struct {
	Name string
	Desc string
	Seed int64
}

// Storms is the catalogue of reproducible storms.
func Storms() []Storm {
	base := []struct{ name, desc string }{
		{"kiln-blackout", "一座窑断电引出的连锁告警"},
		{"label-flap", "标签抖动：同一条告警反复改标签"},
		{"silence-expiry", "静默到期自动失效"},
		{"silence-cancel", "静默提前取消"},
		{"replica-split", "副本之间网络分区"},
		{"night-shift", "跨午夜的夜班路由"},
		{"cross-timezone", "跨时区时间窗判定"},
		{"holiday", "节假日路由"},
		{"fan-cascade", "风机连锁：一台风机带起整窑告警"},
		{"thermocouple-drift", "热电偶漂移与自动恢复"},
		{"plateau", "持续性平台告警反复重发"},
		{"flash-flood", "瞬时洪峰：几百条同时出现"},
		{"zone-sweep", "整片分区扫描式告警"},
		{"slow-burn", "零散慢热：单条逐步累积"},
		{"channel-switch", "渠道切换，旧渠道不重复发"},
		{"retry-storm", "渠道故障与指数退避"},
		{"power-then-temp", "断电先于高温，抑制后恢复"},
		{"inhibit-chain", "两处断电与多目标抑制链"},
		{"mixed-night-holiday", "夜班与节假日同时生效"},
		{"resolve-race", "解除与重发交错"},
		{"stale-timeout", "告警源失联后的超时解除"},
		{"all-kinds", "全部告警类型混发"},
		{"partition-during-flood", "洪峰中副本断网"},
		{"silence-while-firing", "已发告警进入静默"},
	}
	out := make([]Storm, len(base))
	for i, b := range base {
		out[i] = Storm{Name: b.name, Desc: b.desc, Seed: int64(20260101 + i*7919)}
	}
	return out
}

// Config is one scenario's parameters.
type Config struct {
	Name          string
	Start         time.Time
	Duration      time.Duration
	Step          time.Duration
	GroupWait     time.Duration
	GroupInterval time.Duration
	StaleAfter    time.Duration
	RepeatEvery   time.Duration
	Silences      []SilencePlan
	Partitions    []PartitionPlan
	Switches      []engine.ChannelSwitch
	ChannelOutage bool
	Build         func(r *rand.Rand, cfg *Config) []model.AlertEvent
}

// SilencePlan schedules one silence relative to the run start.
type SilencePlan struct {
	ID       string
	StartAt  time.Duration
	EndAt    time.Duration
	CancelAt time.Duration // 0: never cancelled
	Match    model.MatchAll
	NodeID   string
	Reason   string
}

// PartitionPlan schedules one replica partition.
type PartitionPlan struct {
	StartAt time.Duration
	EndAt   time.Duration
	Groups  [][]string
}

// Result is one storm's output.
type Result struct {
	Storm  Storm
	Config Config
	Run    model.RunResult
	Verify verify.Report
}

// BaseConfig returns the standard engine parameters for a scenario.
func BaseConfig(name string, start time.Time) Config {
	return Config{
		Name:          name,
		Start:         start,
		Duration:      3 * time.Hour,
		Step:          15 * time.Second,
		GroupWait:     2 * time.Minute,
		GroupInterval: 5 * time.Minute,
		StaleAfter:    10 * time.Minute,
		RepeatEvery:   2 * time.Minute,
	}
}

// Run executes one storm at its seed.
func Run(st Storm) (Result, error) {
	return RunSeed(st, st.Seed)
}

// RunSeed executes one storm with an explicit seed.
func RunSeed(st Storm, seed int64) (Result, error) {
	cfg := buildConfig(st.Name, seed)
	r := rand.New(rand.NewSource(seed))
	treeSpec := Tree()
	tree, err := routing.Compile(treeSpec)
	if err != nil {
		return Result{}, fmt.Errorf("%s: 编译路由树: %w", st.Name, err)
	}
	tEnd := cfg.Start.Add(cfg.Duration)
	eng := engine.New(engine.Config{
		Step:          cfg.Step,
		GroupWait:     cfg.GroupWait,
		GroupInterval: cfg.GroupInterval,
		StaleAfter:    cfg.StaleAfter,
		Replicas:      Replicas,
		Switches:      cfg.Switches,
	}, tree, cfg.Start, tEnd)

	for _, sp := range cfg.Silences {
		eng.AddSilence(&model.Silence{
			ID:        sp.ID,
			Match:     sp.Match,
			Starts:    cfg.Start.Add(sp.StartAt),
			Ends:      cfg.Start.Add(sp.EndAt),
			NodeID:    sp.NodeID,
			Reason:    sp.Reason,
			CreatedAt: cfg.Start,
		})
	}

	events := cfg.Build(r, &cfg)

	// Cancellations are applied at their instant, before the tick at or after
	// it, so a cancel exactly on a tick boundary takes effect at that tick.
	type cancel struct {
		at time.Time
		id string
	}
	var cancels []cancel
	for _, sp := range cfg.Silences {
		if sp.CancelAt > 0 {
			cancels = append(cancels, cancel{at: cfg.Start.Add(sp.CancelAt), id: sp.ID})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	sort.SliceStable(cancels, func(i, j int) bool { return cancels[i].at.Before(cancels[j].at) })

	ei, ci := 0, 0
	for t := cfg.Start; !t.After(tEnd); t = t.Add(cfg.Step) {
		for ei < len(events) && !events[ei].At.After(t) {
			eng.Ingest(events[ei])
			ei++
		}
		for ci < len(cancels) && !cancels[ci].at.After(t) {
			eng.CancelSilence(cancels[ci].id, cancels[ci].at)
			ci++
		}
		eng.Tick(t)
	}
	eng.Flush(tEnd)

	// Delivery layer: independent of content, replays the replicas.
	net := &channel.Network{}
	for _, p := range cfg.Partitions {
		net.Parts = append(net.Parts, channel.Partition{
			From: cfg.Start.Add(p.StartAt), To: cfg.Start.Add(p.EndAt), Groups: p.Groups,
		})
	}
	disp := channel.NewDispatcher(Replicas, ChannelsFor(cfg), Backoff, net, func(nodeID string, at time.Time) string {
		return currentChannel(tree, nodeID, at, cfg.Switches)
	})
	deliveries := disp.Run(eng.Notifications())

	stats := eng.Stats()
	eng.SetStats(func(s *model.RunStats) {
		s.Retries = disp.Stats.Retries
		s.Duplicates = disp.Stats.Duplicates
		s.HeldDuplicates = disp.Stats.HeldDuplicates
		s.Deliveries = disp.Stats.Delivered
		s.NetworkFailures = disp.Stats.Failures
	})
	stats = eng.Stats()
	stats.Duplicates = disp.Stats.Duplicates
	stats.HeldDuplicates = disp.Stats.HeldDuplicates
	stats.Deliveries = disp.Stats.Delivered
	stats.NetworkFailures = disp.Stats.Failures

	res := buildResult(st, cfg, eng, deliveries, net, seed, tEnd)
	rep := verify.Run(res, cfg.Start, tEnd)
	res.Problems = rep.Problems()
	return Result{Storm: st, Config: cfg, Run: res, Verify: rep}, nil
}

func currentChannel(tree *routing.Tree, nodeID string, at time.Time, switches []engine.ChannelSwitch) string {
	n := tree.ByID[nodeID]
	if n == nil {
		return ""
	}
	ch := n.Spec.Channel
	for _, s := range switches {
		if s.NodeID == nodeID && !at.Before(s.At) {
			ch = s.To
		}
	}
	if ch == "" {
		ch = "默认"
	}
	return ch
}

func buildResult(st Storm, cfg Config, eng *engine.Engine, deliveries []channel.Delivery, net *channel.Network, seed int64, tEnd time.Time) model.RunResult {
	var alerts []model.Alert
	alertEvents := eng.Observed()
	for _, w := range eng.AlertsSorted() {
		alerts = append(alerts, model.Alert{ID: w.ID, Labels: w.Labels, Status: w.Status, Start: w.Start, End: w.End, LastSeen: w.LastSeen})
	}
	var sils []model.SilenceView
	for _, s := range eng.Silences() {
		sils = append(sils, model.SilenceView{Silence: *s, State: silenceState(s, tEnd)})
	}
	var dels []model.Delivery
	for _, d := range deliveries {
		dels = append(dels, model.Delivery{
			NotificationID: d.NotificationID, DedupeID: d.DedupeID, Channel: d.Channel,
			Replica: d.Replica, ReplicaIdx: d.ReplicaIdx, Attempt: d.Round, At: d.At,
			OK: d.OK, Duplicate: d.Duplicate, Reason: d.Reason, Backoff: d.Backoff,
		})
	}
	var parts []model.Partition
	for _, p := range net.Parts {
		parts = append(parts, model.Partition{From: p.From, To: p.To, Groups: p.Groups})
	}
	var sws []model.SwitchInfo
	for _, s := range cfg.Switches {
		sws = append(sws, model.SwitchInfo{NodeID: s.NodeID, At: s.At, To: s.To})
	}
	chans := make([]string, 0, len(Channels()))
	for _, c := range ChannelsFor(cfg) {
		chans = append(chans, c.Name)
	}
	return model.RunResult{
		Name:          st.Name,
		Seed:          seed,
		Start:         cfg.Start,
		End:           tEnd,
		Replicas:      Replicas,
		Alerts:        alerts,
		AlertEvents:   alertEvents,
		Notifications: eng.Notifications(),
		Deliveries:    dels,
		Silences:      sils,
		Events:        eng.Timeline(),
		Stats:         eng.Stats(),
		Tree:          Tree(),
		Channels:      chans,
		Partitions:    parts,
		Batching: model.BatchParams{
			Step: cfg.Step, GroupWait: cfg.GroupWait, GroupInterval: cfg.GroupInterval,
			StaleAfter: cfg.StaleAfter,
		},
		Switches:  sws,
		Snapshots: eng.Snapshots(),
	}
}

func silenceState(s *model.Silence, tEnd time.Time) string {
	if !s.CancelledAt.IsZero() {
		if s.CancelledAt.Before(s.Starts) {
			return "生效前已取消"
		}
		return "已提前取消"
	}
	if !tEnd.Before(s.Ends) {
		return "已到期"
	}
	if s.ActiveStart.IsZero() {
		return "未生效"
	}
	return "生效中"
}
