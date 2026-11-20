package peers

import "testing"

func TestDecodeSandboxLink(t *testing.T) {
	peer := Peer{Host: "dev", URL: "https://dev.example.ts.net:18456"}
	const origin = "https://a37-terra-2-ab12cd34ef.example.ts.net"
	entry := link{EnvironmentID: "sandbox-environment", Status: "unreachable", URLs: []string{origin + "/"}}
	got, ok := decodeSandboxLink(peer, entry)
	want := SandboxLink{
		Peer: "dev", Name: "terra-2", SandboxID: "ab12cd34ef", EnvironmentID: entry.EnvironmentID,
		URL: origin, Status: "unreachable",
	}
	if !ok || got != want {
		t.Fatalf("decoded sandbox link = %+v, %t", got, ok)
	}
	for _, addresses := range [][]string{
		{"https://dev.example.ts.net"},
		{"https://a37-terra-2-ab12cd34ef.other.ts.net"},
		{"https://a37-terra-2-invalid.example.ts.net"},
		{origin + ":443"},
		{origin + "/settings"},
		{origin, "https://alternate.example.ts.net"},
		{origin + "?token=fixture"},
	} {
		entry.URLs = addresses
		if _, ok := decodeSandboxLink(peer, entry); ok {
			t.Errorf("matched unrelated link %q", addresses)
		}
	}
	entry.URLs = []string{origin}
	entry.EnvironmentID = "-invalid"
	if _, ok := decodeSandboxLink(peer, entry); ok {
		t.Error("matched unsafe environment ID")
	}
}
