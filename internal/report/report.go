// Package report prints the verification summary.
package report

import (
	"fmt"
	"io"
	"text/tabwriter"

	"kilnalert/internal/sim"
)

// AllOK reports whether every storm passed.
func AllOK(results []sim.Result) bool {
	for _, r := range results {
		if !r.Verify.OK() {
			return false
		}
	}
	return true
}

// Print writes the summary table and every problem found.
func Print(w io.Writer, results []sim.Result, verbose bool) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "风暴\t告警实例\t源事件\t应发\t实发\t逐条对上\t漏发\t多发\t内容不符\t重复送达\t未送达\t静默\t抑制\t压缩比")
	fail := 0
	for _, r := range results {
		v := r.Verify
		if !v.OK() {
			fail++
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.1fx\n",
			r.Storm.Name, v.Instances, v.SourceEvents, v.Expected, v.Actual, v.Matched,
			len(v.Missing), len(v.Extra), len(v.Mismatched), v.DuplicateDeliveries, len(v.Undelivered),
			v.SuppressedSilence, v.SuppressedInhibit, v.CompressionRatio)
	}
	tw.Flush()
	if fail == 0 {
		fmt.Fprintf(w, "\n全部 %d 个风暴核对通过：每条应发通知都逐条对上，没有漏发；重复只出现在分区期间，且每条通知都至少送达一次。\n", len(results))
		return
	}
	fmt.Fprintf(w, "\n%d / %d 个风暴核对未通过：\n", fail, len(results))
	for _, r := range results {
		problems := r.Verify.Problems()
		if len(problems) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n【%s】%s\n", r.Storm.Name, r.Storm.Desc)
		limit := len(problems)
		if !verbose && limit > 10 {
			limit = 10
		}
		for _, p := range problems[:limit] {
			fmt.Fprintln(w, "  -", p)
		}
		if limit < len(problems) {
			fmt.Fprintf(w, "  …另 %d 条（加 -v 查看全部）\n", len(problems)-limit)
		}
	}
}
