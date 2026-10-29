package main

import (
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildOf(t *testing.T) {
	bi := &debug.BuildInfo{GoVersion: "go1.26.10",
		Deps: []*debug.Module{{Path: "golang.org/x/text", Version: "v0.30.0"}, {Path: "golang.org/x/net", Version: "v0.62.0"}},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "897e1fb"}, {Key: "vcs.modified", Value: "false"}}}
	b := buildOf(bi)
	if b.commit() != "897e1fb" || b.Go != "go1.26.10" || b.XNet != "v0.62.0" {
		t.Errorf("clean: %+v", b)
	}
	bi.Settings[1].Value = "true"
	if c := buildOf(bi).commit(); c != "897e1fb-dirty" {
		t.Errorf("modified tree: %s", c)
	}
	if c := buildOf(&debug.BuildInfo{}).commit(); c != "unknown" {
		t.Errorf("unstamped: %s", c)
	}
}

// The deploy build (DEPLOY.md, "Ship it") stamps the commit it was built
// from, so what is live can be read off the binary.
func TestDeployBuildNamesItsCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skip("not a git checkout:", err)
	}
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(head))
	if len(status) > 0 {
		want += "-dirty"
	}
	bin := filepath.Join(t.TempDir(), "blygd")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\ncommit "+want+"\n") {
		t.Errorf("blygd version says\n%s\nwant commit %s", out, want)
	}
}
