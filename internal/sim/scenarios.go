package sim

import (
	"math/rand"
	"time"

	"kilnalert/internal/engine"
	"kilnalert/internal/model"
)

// alertname and severity vocabularies.
const (
	OverTemp     = "OverTemp"
	Atmosphere   = "Atmosphere"
	Oxygen       = "Oxygen"
	FanFault     = "FanFault"
	PowerLoss    = "PowerLoss"
	Thermocouple = "Thermocouple"
	FanVibration = "FanVibration"
)

var severityOf = map[string]string{
	OverTemp:     "warning",
	Atmosphere:   "warning",
	Oxygen:       "warning",
	FanFault:     "critical",
	PowerLoss:    "critical",
	Thermocouple: "info",
	FanVibration: "warning",
}

var kindOf = map[string]string{
	OverTemp:     "process",
	Atmosphere:   "process",
	Oxygen:       "process",
	FanFault:     "mechanical",
	PowerLoss:    "eletric",
	Thermocouple: "sensor",
	FanVibration: "mechanical",
}

var operators = []string{"甲班", "乙班", "丙班"}

// buildConfig returns a scenario's parameters and event builder.
func buildConfig(name string, seed int64) Config {
	// Each scenario starts at a date that exercises its time windows. Storm
	// dates are fixed so runs are reproducible.
	base := time.Date(2026, 3, 2, 6, 0, 0, 0, time.FixedZone("CST", 8*3600)) // Monday 06:00 Shanghai
	cfg := BaseConfig(name, base)
	switch name {
	case "kiln-blackout", "power-then-temp", "inhibit-chain", "fan-cascade", "flash-flood":
		cfg.Build = buildBlackout
	case "label-flap":
		cfg.Build = buildLabelFlap
	case "silence-expiry", "silence-cancel", "silence-while-firing":
		cfg.Build = buildSilence
		cfg.Silences = silencePlans(name, cfg)
	case "replica-split":
		cfg.Build = buildBlackout
		cfg.Partitions = []PartitionPlan{{
			StartAt: 20 * time.Minute, EndAt: 70 * time.Minute,
			Groups: [][]string{{"kln-0"}, {"kln-1", "kln-2"}},
		}}
	case "night-shift", "mixed-night-holiday":
		cfg.Start = time.Date(2026, 3, 2, 20, 30, 0, 0, time.FixedZone("CST", 8*3600)) // Monday evening
		cfg.Duration = 5 * time.Hour
		cfg.Build = buildMixed
	case "cross-timezone":
		cfg.Start = time.Date(2026, 3, 2, 14, 0, 0, 0, time.FixedZone("CST", 8*3600))
		cfg.Duration = 4 * time.Hour
		cfg.Build = buildMixed
	case "holiday":
		cfg.Start = time.Date(2026, 10, 1, 18, 0, 0, 0, time.FixedZone("CST", 8*3600)) // National Day
		cfg.Duration = 4 * time.Hour
		cfg.Build = buildMixed
	case "thermocouple-drift":
		cfg.Build = buildDrift
	case "plateau", "stale-timeout":
		cfg.Build = buildPlateau
	case "zone-sweep":
		cfg.Build = buildZoneSweep
	case "slow-burn":
		cfg.Build = buildSlowBurn
	case "channel-switch":
		cfg.Build = buildSlowBurn
		cfg.Switches = []engine.ChannelSwitch{
			{NodeID: "zoneA-temp", At: cfg.Start.Add(40 * time.Minute), To: "邮件"},
			{NodeID: "zoneA-fan", At: cfg.Start.Add(70 * time.Minute), To: "电话"},
		}
	case "retry-storm":
		// A channel-wide outage at the start of the run exercises the full
		// exponential backoff ladder before the first message lands.
		cfg.Build = buildFlashFlood
		cfg.Duration = 60 * time.Minute
		cfg.ChannelOutage = true
	case "partition-during-flood":
		cfg.Build = buildFlashFlood
		cfg.Partitions = []PartitionPlan{{
			StartAt: 5 * time.Minute, EndAt: 95 * time.Minute,
			Groups: [][]string{{"kln-0"}, {"kln-1"}, {"kln-2"}},
		}}
	case "resolve-race":
		cfg.Build = buildResolveRace
	case "all-kinds":
		cfg.Build = buildAllKinds
	default:
		cfg.Build = buildBlackout
	}
	return cfg
}

// labels builds an alert's label set.
func labels(alertname string, kiln int, extra map[string]string) map[string]string {
	l := map[string]string{
		"alertname": alertname,
		"kiln":      itoa(kiln),
		"zone":      ZoneOf(kiln),
		"severity":  severityOf[alertname],
		"kind":      kindOf[alertname],
		"operator":  operators[kiln%len(operators)],
		"source":    "probe",
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}

func fanLabels(kiln int, fan string) map[string]string {
	return labels(FanFault, kiln, map[string]string{"fan": fan})
}

func itoa(x int) string {
	if x == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	neg := x < 0
	if neg {
		x = -x
	}
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

// storm is a mutable event collector. Each firing of a label set is an
// episode; an episode runs from its fire time to its resolve time (or the end
// of the scenario). Repeats are generated from the episodes afterwards, so
// the order in which a scenario declares fires and resolves does not matter.
type storm struct {
	r        *rand.Rand
	cfg      *Config
	evs      []model.AlertEvent
	episodes []*episode
	open     map[string]*episode
}

type episode struct {
	labels    map[string]string
	fireAt    time.Time
	resolveAt time.Time
}

func newStorm(r *rand.Rand, cfg *Config) *storm {
	return &storm{r: r, cfg: cfg, open: map[string]*episode{}}
}

func (s *storm) fire(l map[string]string, at time.Time, note string) {
	s.evs = append(s.evs, model.AlertEvent{Labels: clone(l), Fire: true, At: at, Source: "probe", Note: note})
	ep := &episode{labels: clone(l), fireAt: at}
	s.episodes = append(s.episodes, ep)
	s.open[model.Fingerprint(l)] = ep
}

func (s *storm) resolve(l map[string]string, at time.Time, note string) {
	s.evs = append(s.evs, model.AlertEvent{Labels: clone(l), Fire: false, At: at, Source: "probe", Note: note})
	fp := model.Fingerprint(l)
	if ep := s.open[fp]; ep != nil {
		ep.resolveAt = at
		delete(s.open, fp)
	}
}

// repeat re-sends every episode that is live at each repeat instant: fired at
// or before it, not yet resolved at it. This is what a real probe does, and
// it is what keeps a long alarm from being auto-resolved as stale.
func (s *storm) repeat(until time.Time) {
	period := s.cfg.RepeatEvery
	if period <= 0 {
		return
	}
	for t := s.cfg.Start.Add(period); t.Before(until); t = t.Add(period) {
		for _, ep := range s.episodes {
			if ep.fireAt.After(t) {
				continue
			}
			if !ep.resolveAt.IsZero() && !ep.resolveAt.After(t) {
				continue
			}
			s.evs = append(s.evs, model.AlertEvent{Labels: clone(ep.labels), Fire: true, At: t, Source: "probe", Note: "重复上报"})
		}
	}
}

// liveEpisodes returns episodes live at t.
func (s *storm) liveEpisodes(t time.Time) []*episode {
	var out []*episode
	for _, ep := range s.episodes {
		if ep.fireAt.After(t) {
			continue
		}
		if !ep.resolveAt.IsZero() && !ep.resolveAt.After(t) {
			continue
		}
		out = append(out, ep)
	}
	return out
}

func clone(l map[string]string) map[string]string { return model.CloneLabels(l) }

func t0(cfg *Config) time.Time { return cfg.Start }

// buildBlackout: one kiln loses power, which cascades: the kiln's electrical
// and temperature probes trip, fans stop, and every kiln in the zone wobbles.
// This is the "one kiln, hundreds of alarms" storm, and the power-loss
// inhibition rule is what keeps the operator's phone quiet during it.
func buildBlackout(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	kiln := 7 + r.Intn(10) // an A or B zone kiln
	zone := ZoneOf(kiln)

	// Background sensor noise across the plant.
	for i := 0; i < 6; i++ {
		k := 1 + r.Intn(50)
		s.fire(labels(Thermocouple, k, nil), base.Add(time.Duration(i)*30*time.Second), "背景")
	}

	// The trigger.
	trigger := base.Add(10 * time.Minute)
	power := labels(PowerLoss, kiln, nil)
	s.fire(power, trigger, "主电源跳闸")

	// The cascade: everything in the zone that depends on power.
	n := 0
	for k := 1; k <= 50; k++ {
		if ZoneOf(k) != zone || k == kiln {
			continue
		}
		s.fire(labels(OverTemp, k, nil), trigger.Add(time.Minute+time.Duration(n)*7*time.Second), "连锁：温控失效")
		n++
		s.fire(fanLabels(k, "F"+itoa(k%4+1)), trigger.Add(time.Minute+time.Duration(n)*9*time.Second), "连锁：风机停转")
		n++
		if k%2 == 0 {
			s.fire(labels(Atmosphere, k, nil), trigger.Add(time.Minute+time.Duration(n)*11*time.Second), "连锁：气氛异常")
			n++
		}
	}
	// Electrical faults in the same zone: these are what the power-loss
	// rule is meant to swallow. They belong to the kilns that are already
	// showing the cascade, so the rule's labels line up.
	for i, k := range []int{kiln + 1, kiln + 2, kiln + 3} {
		if k > 50 || ZoneOf(k) != zone || k == kiln {
			continue
		}
		s.fire(labels(FanVibration, k, nil), trigger.Add(3*time.Minute+time.Duration(i)*5*time.Second), "电气连锁")
	}
	// The broken kiln's own probes stream sensor faults.
	for i := 0; i < 12; i++ {
		s.fire(labels(Thermocouple, kiln, map[string]string{"probe": itoa(i)}), trigger.Add(2*time.Minute+time.Duration(i)*20*time.Second), "热电偶断线")
	}

	// Power is restored 40 minutes after the trip; the power alarm clears
	// first, then everything else recovers over the next few minutes.
	restore := trigger.Add(40 * time.Minute)
	for _, ep := range s.liveEpisodes(restore) {
		at := restore.Add(time.Duration(len(model.Fingerprint(ep.labels))%7) * time.Minute)
		if ep.labels["alertname"] == PowerLoss {
			at = restore
		}
		s.resolve(ep.labels, at, "恢复")
	}
	s.repeat(base.Add(cfg.Duration - 10*time.Minute))
	return s.evs
}

// buildLabelFlap: one physical problem whose alert labels keep changing, so
// the alert service sees many instances rather than one.
func buildLabelFlap(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	kiln := 3
	base := t0(cfg)
	fans := []string{"F1", "F2", "F3"}
	sevs := []string{"warning", "critical"}
	for i := 0; i < 40; i++ {
		at := base.Add(time.Duration(i) * 45 * time.Second)
		if i > 0 {
			prev := fans[(i-1)%len(fans)]
			s.resolve(fanLabels(kiln, prev), at.Add(-5*time.Second), "标签变化，旧实例解除")
		}
		l := fanLabels(kiln, fans[i%len(fans)])
		l["severity"] = sevs[(i/3)%2]
		s.fire(l, at, "标签抖动")
	}
	s.repeat(base.Add(cfg.Duration - 15*time.Minute))
	return s.evs
}

// buildSilence: a maintenance silence covers five kilns. Some probes trip
// before it, some during, and some after. Alerts that trip inside a silence
// are held (not notified); when the silence ends or is cancelled the still-
// firing ones are reported. One kiln outside the silence keeps notifying so
// the silence's effect is visible by contrast.
func buildSilence(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	covered := func(k int) bool { return k >= 10 && k <= 14 }

	// Before the silence opens (t < 15m): two kilns are already hot.
	for _, k := range []int{10, 11} {
		s.fire(labels(OverTemp, k, map[string]string{"probe": "P1"}), base.Add(4*time.Minute), "维护前已超温")
	}
	// One kiln outside the silenced range.
	s.fire(labels(OverTemp, 3, map[string]string{"probe": "P1"}), base.Add(3*time.Minute), "未被静默的窑")

	// During the silence: every covered kiln trips several probes.
	for i := 0; i < 5; i++ {
		for k := 10; k <= 14; k++ {
			at := base.Add(time.Duration(18+i*8) * time.Minute)
			s.fire(labels(OverTemp, k, map[string]string{"probe": "P" + itoa(2+i)}), at, "维护期间新增")
		}
	}
	// Two early ones recover while still silenced; no notification results.
	for _, k := range []int{10, 11} {
		s.resolve(labels(OverTemp, k, map[string]string{"probe": "P1"}), base.Add(40*time.Minute), "维护中恢复")
	}
	// After the silence: one more kiln trips and is notified normally.
	s.fire(labels(OverTemp, 12, map[string]string{"probe": "P9"}), base.Add(110*time.Minute), "静默结束后新增")
	// The kiln outside the silence recovers late.
	s.resolve(labels(OverTemp, 3, map[string]string{"probe": "P1"}), base.Add(120*time.Minute), "恢复")
	_ = covered
	s.repeat(base.Add(cfg.Duration - 5*time.Minute))
	return s.evs
}

func silencePlans(name string, cfg Config) []SilencePlan {
	switch name {
	case "silence-expiry":
		return []SilencePlan{{
			ID: "sil-1", StartAt: 15 * time.Minute, EndAt: 95 * time.Minute,
			Match:  model.MatchAll{{Name: "kiln", Op: "=~", Value: "1[0-4]"}, {Name: "alertname", Op: "=", Value: OverTemp}},
			Reason: "10–14 号窑换耐火砖",
		}}
	case "silence-cancel":
		return []SilencePlan{{
			ID: "sil-2", StartAt: 15 * time.Minute, EndAt: 150 * time.Minute, CancelAt: 70 * time.Minute,
			Match:  model.MatchAll{{Name: "kiln", Op: "=~", Value: "1[0-4]"}, {Name: "alertname", Op: "=", Value: OverTemp}},
			Reason: "计划停窑，提前恢复后取消",
		}}
	case "silence-while-firing":
		return []SilencePlan{{
			ID: "sil-3", StartAt: 15 * time.Minute, EndAt: 95 * time.Minute,
			Match:  model.MatchAll{{Name: "kiln", Op: "=~", Value: "1[0-4]"}, {Name: "alertname", Op: "=", Value: OverTemp}},
			Reason: "告警已在发，期间进入静默",
		}}
	}
	return nil
}

// buildMixed sends a mix of alert types at all severities.
func buildMixed(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	kinds := []string{OverTemp, Atmosphere, Oxygen, FanFault, PowerLoss, Thermocouple, FanVibration}
	for i := 0; i < 40; i++ {
		k := 1 + r.Intn(50)
		kind := kinds[r.Intn(len(kinds))]
		at := base.Add(time.Duration(r.Intn(int(cfg.Duration/time.Minute))) * time.Minute)
		s.fire(labels(kind, k, nil), at, "巡检")
		if r.Intn(2) == 0 {
			s.resolve(labels(kind, k, nil), at.Add(time.Duration(5+r.Intn(40))*time.Minute), "恢复")
		}
	}
	s.repeat(base.Add(cfg.Duration - 15*time.Minute))
	return s.evs
}

// buildDrift: thermometer readings drift, trip, recover, and drift back.
func buildDrift(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	for k := 1; k <= 12; k++ {
		start := base.Add(time.Duration(k) * 3 * time.Minute)
		s.fire(labels(Thermocouple, k, nil), start, "漂移超差")
		s.resolve(labels(Thermocouple, k, nil), start.Add(25*time.Minute), "校准恢复")
		s.fire(labels(OverTemp, k, nil), start.Add(4*time.Minute), "漂移误报高温")
		s.resolve(labels(OverTemp, k, nil), start.Add(9*time.Minute), "恢复")
	}
	s.repeat(base.Add(cfg.Duration - 15*time.Minute))
	return s.evs
}

// buildPlateau: a long steady-state event. The sources re-send it every two
// minutes, so the only notifications are the delta-carrying batches; a slow
// trickle of new probes joins in through the run.
func buildPlateau(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	for k := 1; k <= 8; k++ {
		s.fire(labels(OverTemp, k, nil), base.Add(time.Duration(k)*2*time.Minute), "平台告警")
	}
	for i := 0; i < 8; i++ {
		k := 8 + i
		s.fire(labels(OverTemp, k, nil), base.Add(time.Duration(25+i*12)*time.Minute), "陆续加入")
	}
	s.repeat(base.Add(cfg.Duration - 10*time.Minute))
	// Only a few resolve before the run ends.
	for k := 1; k <= 4; k++ {
		s.resolve(labels(OverTemp, k, nil), base.Add(cfg.Duration-40*time.Minute), "恢复")
	}
	return s.evs
}

// buildSilenceExpiry keeps a steady stream inside the silence window and one
// outside it, so the suppression and its release are both visible.
func buildSilenceExpiry(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	s.fire(labels(OverTemp, 3, nil), base.Add(5*time.Minute), "未被静默的窑")
	for i := 0; i < 24; i++ {
		k := 10 + i%5
		at := base.Add(time.Duration(5+i*3) * time.Minute)
		s.fire(labels(OverTemp, k, map[string]string{"probe": "P" + itoa(i)}), at, "维护期间超温")
	}
	s.repeat(base.Add(cfg.Duration - 5*time.Minute))
	return s.evs
}

// buildFlashFlood: hundreds of alerts in one minute.
func buildFlashFlood(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg).Add(20 * time.Minute)
	kinds := []string{OverTemp, Atmosphere, Oxygen, FanFault, Thermocouple, FanVibration}
	for i := 0; i < 300; i++ {
		k := 1 + r.Intn(50)
		kind := kinds[r.Intn(len(kinds))]
		at := base.Add(time.Duration(r.Intn(60)) * time.Second)
		l := labels(kind, k, nil)
		if kind == FanFault {
			l["fan"] = "F" + itoa(r.Intn(4)+1)
		}
		s.fire(l, at, "洪峰")
	}
	for _, ep := range s.episodes {
		if ep.resolveAt.IsZero() {
			s.resolve(ep.labels, base.Add(time.Duration(30+len(model.Fingerprint(ep.labels))%60)*time.Minute), "洪峰恢复")
		}
	}
	return s.evs
}

// buildZoneSweep: a sweeping scan across one zone.
func buildZoneSweep(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	zone := "B"
	for k := 1; k <= 50; k++ {
		if ZoneOf(k) != zone {
			continue
		}
		at := base.Add(time.Duration(k) * 40 * time.Second)
		s.fire(labels(OverTemp, k, nil), at, "扫描")
		s.fire(fanLabels(k, "F1"), at.Add(20*time.Second), "扫描")
	}
	s.repeat(base.Add(cfg.Duration - 20*time.Minute))
	return s.evs
}

// buildSlowBurn: one alert at a time, minutes apart.
func buildSlowBurn(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	for i := 0; i < 10; i++ {
		k := 20 + i
		at := base.Add(time.Duration(i) * 9 * time.Minute)
		s.fire(labels(OverTemp, k, nil), at, "慢热")
		s.resolve(labels(OverTemp, k, nil), at.Add(7*time.Minute), "恢复")
	}
	return s.evs
}

// buildResolveRace: resolves and re-fires interleave.
func buildResolveRace(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	for i := 0; i < 20; i++ {
		k := 30 + i%6
		at := base.Add(time.Duration(i) * 3 * time.Minute)
		s.fire(labels(FanFault, k, nil), at, "间歇")
		s.resolve(labels(FanFault, k, nil), at.Add(4*time.Minute), "短暂恢复")
	}
	s.repeat(base.Add(cfg.Duration - 10*time.Minute))
	return s.evs
}

// buildAllKinds is the firehose over every type.
func buildAllKinds(r *rand.Rand, cfg *Config) []model.AlertEvent {
	s := newStorm(r, cfg)
	base := t0(cfg)
	kinds := []string{OverTemp, Atmosphere, Oxygen, FanFault, PowerLoss, Thermocouple, FanVibration}
	for i := 0; i < 120; i++ {
		k := 1 + r.Intn(50)
		kind := kinds[r.Intn(len(kinds))]
		at := base.Add(time.Duration(r.Intn(int(cfg.Duration/time.Minute))) * time.Minute)
		l := labels(kind, k, nil)
		if kind == FanFault || kind == FanVibration {
			l["fan"] = "F" + itoa(r.Intn(4)+1)
		}
		s.fire(l, at, "混发")
		if r.Intn(3) != 0 {
			s.resolve(l, at.Add(time.Duration(5+r.Intn(60))*time.Minute), "恢复")
		}
	}
	s.repeat(base.Add(cfg.Duration - 15*time.Minute))
	return s.evs
}
