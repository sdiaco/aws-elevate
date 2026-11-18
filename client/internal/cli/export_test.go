package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// pagedService answers the history in two pages and records the query.
type pagedService struct {
	*fakeService
	queries []api.ListQuery
}

func (p *pagedService) Grants(_ context.Context, q api.ListQuery) (*api.GrantList, error) {
	p.queries = append(p.queries, q)
	if q.Cursor == "" {
		next := "page2"
		return &api.GrantList{Items: []api.Grant{grantFor("a000000001"), grantFor("a000000002")}, Cursor: &next}, nil
	}
	return &api.GrantList{Items: []api.Grant{grantFor("a000000003")}}, nil
}

var exportNow = time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)

func historyService() *pagedService {
	return &pagedService{fakeService: &fakeService{
		versions: &api.ConfigVersions{Current: ptr(3), Versions: []api.VersionInfo{{Number: 1}, {Number: 2}, {Number: 3, Valid: true}}},
		docs:     map[int]string{1: `{"schemaVersion":1,"rules":[]}`, 2: `{"schemaVersion":1}`, 3: `{"schemaVersion":1,"rules":[]}`},
	}}
}

func TestExportWritesGrantsRulesAndManifest(t *testing.T) {
	svc := historyService()
	dir := filepath.Join(t.TempDir(), "elevate-history")
	m, err := exportHistory(context.Background(), svc, dir, exportNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Grants != 3 || m.RuleVersions != 3 || len(m.Files) != 4 {
		t.Errorf("manifest %+v", m)
	}
	// The whole history the service keeps: 13 months back, from the start of that month.
	q := svc.queries[0]
	if q.Scope != "all" || !q.Closed || q.From != "2025-10-01T00:00:00Z" || q.To != "2026-11-18T22:00:00Z" || svc.queries[1].Cursor != "page2" {
		t.Errorf("queries %+v", svc.queries)
	}
	names, _ := os.ReadDir(dir)
	var got []string
	for _, n := range names {
		got = append(got, n.Name())
	}
	if strings.Join(got, ",") != "grants.jsonl,manifest.json,rules-v00000001.json,rules-v00000002.json,rules-v00000003.json" {
		t.Errorf("files %v", got)
	}
	f, _ := os.Open(filepath.Join(dir, "grants.jsonl"))
	defer f.Close()
	lines := 0
	for s := bufio.NewScanner(f); s.Scan(); lines++ {
		var g api.Grant
		if err := json.Unmarshal(s.Bytes(), &g); err != nil || g.ID == "" {
			t.Errorf("line %q: %v", s.Text(), err)
		}
	}
	if lines != 3 {
		t.Errorf("%d lines", lines)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode())
	}
	if info, _ := os.Stat(filepath.Join(dir, "grants.jsonl")); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", info.Mode())
	}
	if err := verifyExport(dir, m); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestExportRefusesAnExistingNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exportHistory(context.Background(), historyService(), dir, exportNow); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("err %v", err)
	}
	// An empty folder is fine.
	if _, err := exportHistory(context.Background(), historyService(), t.TempDir(), exportNow); err != nil {
		t.Error(err)
	}
}

func TestExportRefusesToDeleteWhenTheRereadDiffers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "h")
	m, err := exportHistory(context.Background(), historyService(), dir, exportNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grants.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyExport(dir, m); err == nil || !strings.Contains(err.Error(), "grants.jsonl does not match manifest.json") {
		t.Errorf("err %v", err)
	}
	os.Remove(filepath.Join(dir, "rules-v00000002.json"))
	if err := verifyExport(dir, m); err == nil {
		t.Error("a missing file passed")
	}
}

func TestExportWarnsAboutPersonalData(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "h")
	if err := exportStep(context.Background(), h.env, historyService(), dir, exportNow); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"✓ Exported 3 grants and 3 rules versions", dir, "contains user names and account ids"} {
		if !strings.Contains(out, want) {
			t.Errorf("misses %q: %q", want, out)
		}
	}
}
