package main

import "testing"

func TestMeshNodeRequiresAssignedPrivateOrigin(t *testing.T) {
	const url = "https://a37-ts-1-ab12cd34ef.example.ts.net"
	proof := sandboxPeer{URL: url, EnvironmentID: "t3-test", PeerProxy: true}
	node, err := meshNode(nil, "ab12cd34ef", "ts-1", proof)
	if err != nil || node.URL != url || node.EnvironmentID != proof.EnvironmentID {
		t.Fatalf("mesh node = %+v, %v", node, err)
	}
	for _, origin := range []string{
		"http://a37-ts-1-ab12cd34ef.example.ts.net",
		"https://a37-ts-1-other12345.example.ts.net",
		"https://a37-ts-1-ab12cd34ef.example.com",
		url + "/api",
		url + ":18456",
	} {
		proof.URL = origin
		if _, err := meshNode(nil, "ab12cd34ef", "ts-1", proof); err == nil {
			t.Errorf("accepted unassigned URL %s", origin)
		}
	}
}
