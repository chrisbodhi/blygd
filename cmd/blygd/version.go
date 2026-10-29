package main

import (
	"fmt"
	"io"
	"runtime/debug"

	"github.com/chrisbodhi/blygd/internal/config"
)

// build is what this binary was built from, as the toolchain stamped it.
// A build from a git checkout records the commit and whether the tree had
// changes (untracked files count); `go run` and test binaries record none.
type build struct {
	Commit   string // full hash, or "" when unstamped
	Modified bool
	Go       string
	XNet     string // golang.org/x/net, which DEPLOYS.md tracks for advisories
}

func readBuild() build {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return build{}
	}
	return buildOf(bi)
}

func buildOf(bi *debug.BuildInfo) build {
	b := build{Go: bi.GoVersion}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	for _, d := range bi.Deps {
		if d.Path == "golang.org/x/net" {
			b.XNet = d.Version
			if d.Replace != nil {
				b.XNet = d.Replace.Version
			}
		}
	}
	return b
}

// commit is the hash for logs: "unknown" when unstamped, and marked when
// the tree it came from had changes, so no one takes it for that commit.
func (b build) commit() string {
	switch {
	case b.Commit == "":
		return "unknown"
	case b.Modified:
		return b.Commit + "-dirty"
	}
	return b.Commit
}

// write is `blygd version`: one "key value" line each, for scripts.
func (b build) write(w io.Writer) {
	fmt.Fprintf(w, "blygd %s\ncommit %s\ngo %s\nx/net %s\n", config.Version, b.commit(), b.Go, b.XNet)
}
