package main

import (
	"testing"
)

func TestParseTimingsAndUpOutput(t *testing.T) {
	diag := `
  › preflight · Preflight · 0.612s
  › ts-1 · Agent37 creation · 14.305s
  › ts-1 · Guest startup · 18.420s
  › ts-1 · guest t3 · 2.110s
  › ts-1 · guest tailnet · 6.450s
  › ts-1 · guest workspace · 11.200s
  › ts-1 · guest projects · 1.850s
  › ts-1 · guest tools · 3.120s
  › ts-1 Reused 7 baked worktrees
  › ts-1 · MagicDNS resolved · 0.120s
  › ts-1 · Tailscale TCP connected · 0.340s
  › ts-1 · Trusted TLS connected · 4.210s
  › ts-1 · Tailscale HTTPS verification · 4.280s
  › ts-1 · Peer linking · 3.550s
  › up · Command elapsed · 41.200s
`
	flat, per, seeded := parseTimings(diag)
	if seeded != 7 {
		t.Fatalf("seeded = %d, want 7", seeded)
	}
	if flat["Agent37 creation"] != 14.305 || flat["guest workspace"] != 11.2 || flat["Peer linking"] != 3.55 {
		t.Fatalf("unexpected flat timings: %v", flat)
	}
	if per["ts-1"]["guest t3"] != 2.11 {
		t.Fatalf("unexpected per-sandbox timings: %v", per)
	}

	singleOut := "Cloud T3 ready (abc123def4, env-uuid-1).\nT3 Code on Tailscale https://a37-ts-1-abc123def4.tail.example.ts.net\n"
	sbs := parseUpOutput(1, singleOut)
	if len(sbs) != 1 || sbs[0].ID != "abc123def4" || sbs[0].Name != "ts-1" || sbs[0].EnvironmentID != "env-uuid-1" {
		t.Fatalf("unexpected single parse: %+v", sbs)
	}

	batchOut := `{"sandboxes":[{"id":"aaaaaaaaaa","name":"ts-1","t3_url":"https://a37-ts-1-aaaaaaaaaa.tail.example.ts.net","environment_id":"e1"}],"mesh_pending":false}`
	bsbs := parseUpOutput(2, batchOut)
	if len(bsbs) != 1 || bsbs[0].Name != "ts-1" || bsbs[0].ID != "aaaaaaaaaa" {
		t.Fatalf("unexpected batch parse: %+v", bsbs)
	}
}

func TestReadinessReportEvaluation(t *testing.T) {
	r := readinessReport{
		HTTPSReachable:   true,
		PeerLinksCapable: true,
		SkillsLinked:     true,
		OrchCurrent:      true,
		ProvidersReady:   true,
		GitHubReady:      true,
	}
	if !r.ready(false, true) {
		t.Fatal("expected ready without desktop")
	}
	if r.ready(true, true) {
		t.Fatal("expected not ready when desktop required but unconfirmed")
	}
	r.DesktopConfirmed = true
	if !r.ready(true, true) {
		t.Fatal("expected ready with desktop confirmed")
	}
	r.E2ESteps = []e2eStep{{Name: "opencode turn", Passed: true, Seconds: 4.2}}
	if !r.ready(true, true) {
		t.Fatal("expected ready with passing E2E step")
	}
	r.E2ESteps = append(r.E2ESteps, e2eStep{Name: "claude turn", Passed: false, Seconds: 2.1})
	if r.ready(true, true) {
		t.Fatal("expected not ready with failing E2E step")
	}
	r.E2ESteps = nil
	r.MissingPeers = []string{"peer-a"}
	if r.ready(false, true) {
		t.Fatal("expected not ready with missing peer")
	}
}
