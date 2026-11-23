package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"kilnalert/internal/sim"
)

func loadOne(t *testing.T) sim.Result {
	t.Helper()
	var st sim.Storm
	for _, s := range sim.Storms() {
		if s.Name == "kiln-blackout" {
			st = s
		}
	}
	res, err := sim.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestViewHasAllPanes(t *testing.T) {
	res := loadOne(t)
	srv := New([]sim.Result{res})
	req := httptest.NewRequest("GET", "/api/view?storm=kiln-blackout&t=1500", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var v view
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Tree) == 0 {
		t.Fatal("routing tree missing")
	}
	if len(v.Groups) == 0 {
		t.Fatal("no groups at mid-run")
	}
	if len(v.Notifs) == 0 {
		t.Fatal("no notifications at mid-run")
	}
	foundSuppressed := false
	for _, g := range v.Groups {
		if len(g.Suppressed) > 0 {
			foundSuppressed = true
			if g.Suppressed[0].Reason == "" {
				t.Fatal("suppressed alert must carry a reason")
			}
		}
	}
	if !foundSuppressed {
		t.Fatal("expected at least one inhibited alert mid-cascade")
	}
	if !v.Verify.OK {
		t.Fatalf("run should verify: %v", v.Verify.Problems)
	}
}

func TestViewAtStartIsQuiet(t *testing.T) {
	res := loadOne(t)
	srv := New([]sim.Result{res})
	req := httptest.NewRequest("GET", "/api/view?storm=kiln-blackout&t=0", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var v view
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Notifs) != 0 {
		t.Fatalf("no notification should exist at t=0, got %d", len(v.Notifs))
	}
}

func TestIndexAndStatic(t *testing.T) {
	res := loadOne(t)
	srv := New([]sim.Result{res})
	for _, path := range []string{"/", "/static/app.js", "/static/style.css"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

func TestNodeChannelSwitch(t *testing.T) {
	var st sim.Storm
	for _, s := range sim.Storms() {
		if s.Name == "channel-switch" {
			st = s
		}
	}
	res, err := sim.Run(st)
	if err != nil {
		t.Fatal(err)
	}
	srv := New([]sim.Result{res})
	req := httptest.NewRequest("GET", "/api/view?storm=channel-switch&t=99999", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var v view
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	for _, n := range v.Tree {
		if n.ID == "zoneA-temp" && n.ChannelNow != "邮件" {
			t.Fatalf("zoneA-temp should have switched to 邮件, got %q", n.ChannelNow)
		}
	}
}
