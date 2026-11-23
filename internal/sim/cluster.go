// Package sim builds the ceramic-workshop scenario: fifty kilns with their
// fans, temperatures, atmospheres and power feeds, a routing tree, inhibition
// rules, silences and a set of reproducible alert storms.
package sim

import (
	"kilnalert/internal/model"
	"kilnalert/internal/timewin"
)

// ZoneOf returns the plant zone a kiln belongs to.
func ZoneOf(k int) string {
	switch {
	case k <= 18:
		return "A"
	case k <= 36:
		return "B"
	}
	return "C"
}

func eq(name, value string) model.Matcher { return model.Matcher{Name: name, Op: "=", Value: value} }
func re(name, expr string) model.Matcher  { return model.Matcher{Name: name, Op: "=~", Value: expr} }

// Tree builds the routing tree:
//
//	根 (group by alertname, 看板)
//	├─ 夜班值守 (critical, 22:00-06:00 Asia/Shanghai, 继续匹配兄弟)   → 电话
//	├─ 节假日加固 (critical, 节假日整天, 继续匹配兄弟)                → 电话
//	├─ 断电总控 (alertname=PowerLoss)                               → 电话
//	│   抑制：同 zone 的电气类告警、同 zone 的高温告警
//	└─ 窑炉 (kiln=~[0-9]+)
//	   ├─ A区窑 (zone=A)
//	   │  ├─ 高温 (alertname=OverTemp)                             → 短信（critical 转电话）
//	   │  ├─ 气氛 (alertname=Atmosphere|Oxygen)                    → 邮件
//	   │  └─ 风机 (alertname=FanFault)                             → 短信
//	   ├─ B区窑 ...
//	   └─ C区窑 ...
//
// The night and holiday pages match first and continue, so a critical alert
// at night reaches both its kiln group and the duty operator.
func Tree() *model.RouteNode {
	root := &model.RouteNode{ID: "root", Name: "根", GroupBy: []string{"alertname"}, Channel: "看板"}
	root.Children = []*model.RouteNode{
		{
			ID: "night", Name: "夜班值守", Parent: "root",
			Match:            model.MatchAll{eq("severity", "critical")},
			GroupBy:          []string{"operator"},
			Channel:          "电话",
			ContinueSiblings: true,
			TimeWindows:      []model.TimeWindow{nightShiftWindow()},
		},
		{
			ID: "holiday", Name: "节假日加固", Parent: "root",
			Match:            model.MatchAll{eq("severity", "critical")},
			GroupBy:          []string{"zone"},
			Channel:          "电话",
			ContinueSiblings: true,
			TimeWindows:      []model.TimeWindow{holidayShiftWindow()},
		},
		{
			ID: "power", Name: "断电总控", Parent: "root",
			Match:   model.MatchAll{eq("alertname", "PowerLoss")},
			GroupBy: []string{"zone", "alertname"},
			Channel: "电话",
			InhibitRules: []model.InhibitRule{
				{
					ID:     "power-mutes-electric",
					Source: model.MatchAll{eq("alertname", "PowerLoss")},
					Target: model.MatchAll{eq("kind", "eletric")},
					Equal:  []string{"zone"},
					Note:   "同区断电期间，电气类告警是连锁噪声",
				},
				{
					ID:     "power-mutes-temperature",
					Source: model.MatchAll{eq("alertname", "PowerLoss")},
					Target: model.MatchAll{eq("alertname", "OverTemp")},
					Equal:  []string{"zone"},
					Note:   "断电后温度读数不可信，抑制高温告警",
				},
			},
		},
		{
			ID: "kilns", Name: "窑炉", Parent: "root",
			Match:   model.MatchAll{re("kiln", "[0-9]+")},
			GroupBy: []string{"alertname"},
			Channel: "看板",
			Children: []*model.RouteNode{
				zoneNode("zoneA", "A区窑", "A", "kilns"),
				zoneNode("zoneB", "B区窑", "B", "kilns"),
				zoneNode("zoneC", "C区窑", "C", "kilns"),
			},
		},
	}
	return root
}

func nightShiftWindow() model.TimeWindow {
	w, err := timewin.NewWindow("22:00", "06:00", nil, "Asia/Shanghai", nil)
	if err != nil {
		panic(err)
	}
	return w
}

func holidayShiftWindow() model.TimeWindow {
	w, err := timewin.NewWindow("00:00", "24:00", nil, "Asia/Shanghai", Holidays2026())
	if err != nil {
		panic(err)
	}
	return w
}

func zoneNode(id, name, zone, parent string) *model.RouteNode {
	return &model.RouteNode{
		ID: id, Name: name, Parent: parent,
		Match:   model.MatchAll{eq("zone", zone)},
		GroupBy: []string{"alertname"},
		Channel: "看板",
		Children: []*model.RouteNode{
			{
				// All over-temperature probes in a zone form one group: a
				// zone-wide thermal event is one thing, not N kilns' worth.
				ID: id + "-temp", Name: "高温超限", Parent: id,
				Match:   model.MatchAll{eq("alertname", "OverTemp")},
				GroupBy: []string{"alertname"},
				Channel: "短信",
			},
			{
				ID: id + "-atm", Name: "气氛异常", Parent: id,
				Match:   model.MatchAll{re("alertname", "Atmosphere|Oxygen")},
				GroupBy: []string{"alertname"},
				Channel: "邮件",
			},
			{
				// Fan faults stay per kiln and per fan: each needs its own
				// maintenance ticket.
				ID: id + "-fan", Name: "风机故障", Parent: id,
				Match:   model.MatchAll{eq("alertname", "FanFault")},
				GroupBy: []string{"kiln", "fan"},
				Channel: "短信",
			},
		},
	}
}

// Holidays2026 is the holiday calendar used by holiday-gated routing.
func Holidays2026() map[string]bool {
	h := map[string]bool{}
	for _, d := range []string{
		"2026-01-01", "2026-02-16", "2026-02-17", "2026-02-18", "2026-02-19", "2026-02-20",
		"2026-04-05", "2026-05-01", "2026-06-19", "2026-09-25", "2026-10-01", "2026-10-02",
		"2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07",
	} {
		h[d] = true
	}
	return h
}

// LabelDoc documents one label.
type LabelDoc struct{ Name, Meaning string }

// Labels returns the alert label vocabulary for the UI.
func Labels() []LabelDoc {
	return []LabelDoc{
		{"alertname", "告警类型：OverTemp 高温 / Atmosphere 气氛 / Oxygen 氧含量 / FanFault 风机 / PowerLoss 断电 / Thermocouple 热电偶 / FanVibration 风机振动"},
		{"kiln", "窑号 1–50"},
		{"zone", "分区 A/B/C"},
		{"fan", "风机编号"},
		{"severity", "critical / warning / info"},
		{"kind", "eletric 电气 / mechanical 机械 / sensor 传感器 / process 工艺"},
		{"operator", "值班班组"},
		{"source", "上报来源（模拟探针）"},
	}
}
