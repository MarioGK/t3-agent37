package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestDesktopPairingURLPreservesPrivateOriginAndFragment(t *testing.T) {
	const pairing = "https://a37-test.example.ts.net/pair#token=fixture%2Btoken"
	link, err := url.Parse(desktopPairingURL(pairing))
	if err != nil {
		t.Fatal(err)
	}
	if link.Scheme != "t3code" || link.Host != "pair" || link.Query().Get("pairingUrl") != pairing {
		t.Fatal("incorrect T3 desktop pairing link")
	}
}

func TestLinkDryRunListsAllManagedSandboxesOrOneByName(t *testing.T) {
	t.Setenv(agent37KeyName, "desktop-test")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = namingTestTransport(func(request *http.Request) string {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/instances" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return `{"data":[
			{"id":"sandbox002","name":"ts-2","status":"stopped","metadata":{"managed_by":"t3-agent37","environment":"ts-2"}},
			{"id":"sandbox001","name":"ts-1","status":"running","metadata":{"managed_by":"t3-agent37","environment":"ts-1"}},
			{"id":"sandbox003","name":"other","status":"running"},
			{"id":"sandbox004","name":"ts-4","status":"deleted","metadata":{"managed_by":"t3-agent37","environment":"ts-4"}}
		]}`
	})
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"link", "--dry-run"}, []string{"ts-1", "ts-2"}},
		{[]string{"link", "ts-2", "--dry-run"}, []string{"ts-2"}},
	} {
		var output bytes.Buffer
		if err := run(t.Context(), test.args, &output, &output); err != nil {
			t.Fatal(err)
		}
		var rows []struct{ Name string }
		if err := json.Unmarshal(output.Bytes(), &rows); err != nil || len(rows) != len(test.want) {
			t.Fatalf("link preview = %s, %v", output.String(), err)
		}
		for i, want := range test.want {
			if rows[i].Name != want {
				t.Errorf("link preview target = %q, want %q", rows[i].Name, want)
			}
		}
	}
}
