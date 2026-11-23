package verify_test

import (
	"testing"

	"kilnalert/internal/sim"
)

// TestStormCoverage checks that the catalogue as a whole exercises every
// mechanism the brief names: inhibition, silencing, expiry and cancellation,
// partitions with duplicates, pre-midnight and post-midnight routing,
// cross-timezone windows, holidays, channel switching, retries and the
// auto-resolve timeout. A storm that silently stops covering a mechanism
// should fail this test rather than pass quietly.
func TestStormCoverage(t *testing.T) {
	results, err := sim.RunAll(sim.Storms())
	if err != nil {
		t.Fatal(err)
	}
	type need struct {
		name string
		seen bool
	}
	cov := map[string]*need{
		"抑制":    {name: "规则抑制生效"},
		"静默":    {name: "静默抑制生效"},
		"静默到期":  {name: "静默到期失效"},
		"静默取消":  {name: "静默被提前取消"},
		"分区重复":  {name: "分区期间重复送达"},
		"无分区重复": {name: "分区之外没有重复"},
		"夜班路由":  {name: "夜班时段路由到电话"},
		"节假日路由": {name: "节假日路由"},
		"跨时区":   {name: "跨时区时间窗"},
		"渠道切换":  {name: "渠道切换"},
		"重试成功":  {name: "渠道故障后重试送达"},
		"超时解除":  {name: "重发超时自动解除"},
		"解除通知":  {name: "解除也发通知"},
		"新增增量":  {name: "通知携带新增"},
		"首条基线":  {name: "分组首条通知"},
		"压缩":    {name: "压缩比大于 1"},
	}
	check := func(key string, ok bool) {
		if ok {
			cov[key].seen = true
		}
	}
	for _, r := range results {
		run := r.Run
		if r.Verify.SuppressedInhibit > 0 {
			check("抑制", true)
		}
		if r.Verify.SuppressedSilence > 0 {
			check("静默", true)
		}
		for _, s := range run.Silences {
			if s.State == "已到期" {
				check("静默到期", true)
			}
			if s.State == "已提前取消" {
				check("静默取消", true)
			}
		}
		if r.Verify.DuplicateDeliveries > 0 {
			check("分区重复", true)
		}
		if len(run.Partitions) > 0 && len(r.Verify.DuplicatesOutsidePartition) == 0 {
			check("无分区重复", true)
		}
		for _, n := range run.Notifications {
			if n.NodeID == "night" {
				check("夜班路由", true)
			}
			if n.NodeID == "holiday" {
				check("节假日路由", true)
			}
			if len(n.Delta.Added) > 0 {
				check("新增增量", true)
			}
			if n.Ordinal == 0 {
				check("首条基线", true)
			}
			if len(n.Delta.Resolved) > 0 {
				check("解除通知", true)
			}
		}
		if len(run.Switches) > 0 {
			check("渠道切换", true)
		}
		if run.Stats.Retries > 0 && len(r.Verify.Undelivered) == 0 {
			check("重试成功", true)
		}
		for _, e := range run.Events {
			if e.Kind == "auto_resolve" {
				check("超时解除", true)
			}
		}
		if r.Verify.CompressionRatio > 1 {
			check("压缩", true)
		}
		if r.Storm.Name == "cross-timezone" {
			check("跨时区", true)
		}
	}
	for key, n := range cov {
		if !n.seen {
			t.Errorf("没有任何风暴覆盖：%s", n.name)
		}
		_ = key
	}
}
