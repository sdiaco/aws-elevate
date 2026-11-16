package release

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// releasesJSON is the GitHub answer, newest first, with a draft, a prerelease and a
// release without the header lines.
const releasesJSON = `[
 {"tag_name":"v0.3.0","name":"Faster revocation","html_url":"https://example.com/r/v0.3.0","draft":false,"prerelease":false,
  "body":"type: feature\r\nminInfra: 0.3.0\r\nminClient: 0.2.0\r\n\r\nNotes."},
 {"tag_name":"v0.2.1","name":"Fix approval","html_url":"https://example.com/r/v0.2.1","draft":false,"prerelease":false,
  "body":"type: security\nminInfra: 0.1.0\nminClient: 0.1.0\n\nNotes."},
 {"tag_name":"v0.2.0","name":"Draft","html_url":"","draft":true,"prerelease":false,"body":"type: feature"},
 {"tag_name":"v0.2.0-rc.1","name":"RC","html_url":"","draft":false,"prerelease":true,"body":"type: feature"},
 {"tag_name":"v0.1.1","name":"No header","html_url":"https://example.com/r/v0.1.1","draft":false,"prerelease":false,"body":"Just notes."},
 {"tag_name":"v0.1.0","name":"First","html_url":"https://example.com/r/v0.1.0","draft":false,"prerelease":false,
  "body":"type: feature\nminInfra: 0.1.0\nminClient: 0.1.0"}
]`

func serve(t *testing.T, status int, body string) *Lister {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("accept %q", r.Header.Get("Accept"))
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Lister{HTTP: srv.Client(), URL: srv.URL}
}

func versions(rels []Release) string {
	v := make([]string, len(rels))
	for i, r := range rels {
		v[i] = r.Version
	}
	return strings.Join(v, ",")
}

func TestListReadsTypeAndMinimumsAndSkipsDraftsAndPrereleases(t *testing.T) {
	rels, err := serve(t, 200, releasesJSON).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(rels); got != "0.3.0,0.2.1,0.1.1,0.1.0" {
		t.Fatalf("versions %s", got)
	}
	want := Release{Version: "0.3.0", Type: "feature", Name: "Faster revocation", URL: "https://example.com/r/v0.3.0", MinInfra: "0.3.0", MinClient: "0.2.0"}
	if rels[0] != want {
		t.Errorf("got %+v\nwant %+v", rels[0], want)
	}
	if rels[1].Type != "security" || rels[2].Type != "" || rels[2].MinInfra != "" {
		t.Errorf("header lines: %+v %+v", rels[1], rels[2])
	}
}

func TestListFailsWithTheStatus(t *testing.T) {
	_, err := serve(t, 404, `{"message":"Not Found"}`).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err %v", err)
	}
}

func TestClientUpdatesAreNewerAndWorkWithTheInfrastructure(t *testing.T) {
	rels, _ := serve(t, 200, releasesJSON).List(context.Background())
	// infra 0.2.0: 0.3.0 needs infra 0.3.0; 0.1.1 says nothing, so it is not proposed.
	if got := versions(ClientUpdates(rels, "0.1.0", "0.2.0")); got != "0.2.1" {
		t.Errorf("client updates %s", got)
	}
	if got := versions(ClientUpdates(rels, "0.1.0", "0.3.0")); got != "0.3.0,0.2.1" {
		t.Errorf("client updates %s", got)
	}
	if got := versions(ClientUpdates(rels, "0.3.0", "0.3.0")); got != "" {
		t.Errorf("up to date: %s", got)
	}
}

func TestInfraUpdatesAreNewerThanTheInfrastructure(t *testing.T) {
	rels, _ := serve(t, 200, releasesJSON).List(context.Background())
	if got := versions(InfraUpdates(rels, "0.2.0")); got != "0.3.0,0.2.1" {
		t.Errorf("infra updates %s", got)
	}
}

// GitHub orders by date: a patch of an older line published later must not come first.
func TestUpdatesAreSortedByVersion(t *testing.T) {
	rels := []Release{
		{Version: "0.1.3", MinInfra: "0.0.1"},
		{Version: "0.2.0", MinInfra: "0.0.1"},
		{Version: "0.1.2", MinInfra: "0.0.1"},
	}
	if got := versions(ClientUpdates(rels, "0.1.0", "0.1.0")); got != "0.2.0,0.1.3,0.1.2" {
		t.Errorf("client updates %s", got)
	}
	if got := versions(InfraUpdates(rels, "0.1.0")); got != "0.2.0,0.1.3,0.1.2" {
		t.Errorf("infra updates %s", got)
	}
}
