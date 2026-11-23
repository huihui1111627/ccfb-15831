// Command kilnalert simulates the ceramic workshop's alarm service, runs the
// reproducible alert storms through it, verifies every notification against
// the rules and serves a page that shows how alerts travelled.
package main

import (
	"flag"
	"fmt"
	"os"

	"kilnalert/internal/report"
	"kilnalert/internal/sim"
	"kilnalert/internal/web"
)

func main() {
	var (
		listen  = flag.String("listen", "127.0.0.1:8909", "HTTP 监听地址")
		check   = flag.Bool("check", false, "只跑全部风暴并核对，不启动页面")
		only    = flag.String("storm", "", "只跑指定名称的风暴")
		verbose = flag.Bool("v", false, "打印每条核对差异")
	)
	flag.Parse()

	storms := sim.Storms()
	if *only != "" {
		var picked []sim.Storm
		for _, s := range storms {
			if s.Name == *only {
				picked = append(picked, s)
			}
		}
		if len(picked) == 0 {
			fmt.Fprintf(os.Stderr, "未知风暴 %q\n", *only)
			os.Exit(2)
		}
		storms = picked
	}

	results, err := sim.RunAll(storms)
	if err != nil {
		fmt.Fprintln(os.Stderr, "运行失败:", err)
		os.Exit(1)
	}
	report.Print(os.Stdout, results, *verbose)
	if *check {
		if !report.AllOK(results) {
			os.Exit(1)
		}
		return
	}
	if err := web.Serve(*listen, results); err != nil {
		fmt.Fprintln(os.Stderr, "启动页面失败:", err)
		os.Exit(1)
	}
}
