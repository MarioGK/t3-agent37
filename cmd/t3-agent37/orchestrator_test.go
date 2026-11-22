package main

import (
	"debug/buildinfo"
	"runtime/debug"
	"strings"
	"testing"
)

func TestSharedOrchestratorRequiresExactPlatformAndCleanRevision(t *testing.T) {
	target := orchestratorTarget{Revision: strings.Repeat("a", 40), Arch: "amd64", GoVersion: "go1.27.4"}
	info := buildinfo.BuildInfo{
		GoVersion: target.GoVersion,
		Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: target.Arch},
			{Key: "vcs.revision", Value: target.Revision}, {Key: "vcs.modified", Value: "false"},
		},
	}
	if !target.valid() || !target.matches(&info) {
		t.Fatal("matching build was rejected")
	}
	for _, candidate := range []orchestratorTarget{
		{Revision: "../bad", Arch: target.Arch, GoVersion: target.GoVersion},
		{Revision: target.Revision, Arch: "arm64", GoVersion: target.GoVersion},
		{Revision: target.Revision, Arch: target.Arch, GoVersion: "go1.27.5"},
	} {
		if candidate.matches(&info) {
			t.Fatalf("accepted an artifact for a different target: %+v", candidate)
		}
	}
	info.Settings[3].Value = "true"
	if target.matches(&info) {
		t.Fatal("accepted a dirty build")
	}
}
