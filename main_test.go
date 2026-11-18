package main

import (
	"context"
	"errors"
	"testing"
)

func TestValidBrewName(t *testing.T) {
	ok := []string{"mysql", "postgresql@18", "font-liberation", "php", "a.b_c+d"}
	bad := []string{"", "../mysql", "my sql", "brew;rm", "name/extra", "name\\extra"}
	for _, name := range ok {
		if !validBrewName(name) {
			t.Fatalf("expected %q to be valid", name)
		}
	}
	for _, name := range bad {
		if validBrewName(name) {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
}

func TestServiceAndDockerControls(t *testing.T) {
	status, tone, actions := serviceControls("none")
	if status != "Stopped" || tone != "warn" || actions[0] != "start" {
		t.Fatalf("none status = %s %s %v", status, tone, actions)
	}
	status, tone, actions = serviceControls("started")
	if status != "Running" || tone != "ok" || len(actions) != 2 {
		t.Fatalf("started status = %s %s %v", status, tone, actions)
	}
	status, tone, _ = serviceControls("error")
	if status != "Error" || tone != "bad" {
		t.Fatalf("error status = %s %s", status, tone)
	}

	status, tone, actions = dockerControls("exited")
	if status != "Stopped" || actions[0] != "start" {
		t.Fatalf("exited container = %s %v", status, actions)
	}
	status, _, actions = dockerControls("running")
	if status != "Running" || actions[0] != "restart" {
		t.Fatalf("running container = %s %v", status, actions)
	}
	status, _, actions = dockerControls("paused")
	if status != "Paused" || actions[0] != "start" {
		t.Fatalf("paused container = %s %v", status, actions)
	}
}

func TestBelongsToApp(t *testing.T) {
	const path = "/Applications/Cursor.app"
	if !belongsToApp(path+"/Contents/MacOS/Cursor", path) {
		t.Fatal("expected process inside the bundle to match")
	}
	if belongsToApp("/Applications/Cursor.app Backup/MacOS/Cursor", path) {
		t.Fatal("a similarly named path must not match")
	}
	if belongsToApp("", path) || belongsToApp(path+"/Contents", "") {
		t.Fatal("empty values must not match")
	}
}

func TestCleanErr(t *testing.T) {
	if got := cleanErr(nil); got != "" {
		t.Fatalf("nil error = %q", got)
	}
	if got := cleanErr(context.DeadlineExceeded); got != "The command timed out" {
		t.Fatalf("timeout = %q", got)
	}
	if got := cleanErr(errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")); got != "Docker isn't running" {
		t.Fatalf("docker = %q", got)
	}
}

func TestVersionForTapFormula(t *testing.T) {
	versions := map[string]string{"timescaledb": "2.29.2", "mysql": "9.6.0"}
	if got := versionFor(versions, "timescale/tap/timescaledb"); got != "2.29.2" {
		t.Fatalf("tap version = %q", got)
	}
	if got := versionFor(versions, "mysql"); got != "9.6.0" {
		t.Fatalf("formula version = %q", got)
	}
	if got := versionFor(versions, "missing"); got != "" {
		t.Fatalf("missing version = %q", got)
	}
}

func TestProgramOrderPrefersProblemsThenRunning(t *testing.T) {
	items := []Program{
		{Name: "zeta", Kind: "App", Tone: "muted"},
		{Name: "mysql", Kind: "Service", Tone: "warn"},
		{Name: "Cursor", Kind: "App", Tone: "ok"},
		{Name: "redis", Kind: "Service", Tone: "bad"},
	}
	if !programLess(items[3], items[2]) || !programLess(items[2], items[1]) || !programLess(items[1], items[0]) {
		t.Fatal("expected error, then running, then stopped, then installed")
	}
}
