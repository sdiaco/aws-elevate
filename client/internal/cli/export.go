package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
)

// Manifest lists what an export holds; Files maps each file name to its SHA-256.
type Manifest struct {
	ExportedAt   time.Time         `json:"exportedAt"`
	Grants       int               `json:"grants"`
	RuleVersions int               `json:"ruleVersions"`
	Files        map[string]string `json:"files"`
}

const manifestFile = "manifest.json"

// exportHistory writes the closed grants of the whole kept history (one JSON line each,
// grants.jsonl), every rules version (rules-v<n>.json) and manifest.json into dir, which
// must not exist or be empty. Only the owner can read them: they hold user names.
func exportHistory(ctx context.Context, svc Service, dir string, now time.Time) (Manifest, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return Manifest{}, fmt.Errorf("%s already exists and is not empty: choose another folder", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Manifest{}, err
	}
	m := Manifest{ExportedAt: now.UTC(), Files: map[string]string{}}
	write := func(name string, data []byte) error {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		m.Files[name] = hex.EncodeToString(sum[:])
		return nil
	}

	// The export asks for all the history the service keeps: without a period the service
	// answers the last 30 days only.
	me, err := svc.Me(ctx)
	if err != nil {
		return m, fmt.Errorf("read how long the history is kept: %w", err)
	}
	end := now.UTC()
	start := time.Date(end.Year(), end.Month()-time.Month(api.HistoryMonths(*me)), 1, 0, 0, 0, 0, time.UTC)
	q := api.ListQuery{Scope: "all", Closed: true, From: start.Format(time.RFC3339), To: end.Format(time.RFC3339)}
	var grants bytes.Buffer
	for {
		page, err := svc.Grants(ctx, q)
		if err != nil {
			return m, fmt.Errorf("read the grant history: %w", err)
		}
		for _, g := range page.Items {
			line, err := json.Marshal(g)
			if err != nil {
				return m, err
			}
			grants.Write(append(line, '\n'))
			m.Grants++
		}
		if page.Cursor == nil {
			break
		}
		q.Cursor = *page.Cursor
	}
	if err := write("grants.jsonl", grants.Bytes()); err != nil {
		return m, err
	}

	versions, err := svc.ConfigVersions(ctx)
	if err != nil {
		return m, fmt.Errorf("read the rules versions: %w", err)
	}
	for _, info := range versions.Versions {
		v, err := svc.ConfigVersion(ctx, info.Number)
		if err != nil {
			return m, fmt.Errorf("read rules version %d: %w", info.Number, err)
		}
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return m, err
		}
		if err := write(fmt.Sprintf("rules-v%08d.json", info.Number), append(data, '\n')); err != nil {
			return m, err
		}
		m.RuleVersions++
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	return m, os.WriteFile(filepath.Join(dir, manifestFile), append(data, '\n'), 0o600)
}

// verifyExport reads the export back: every file listed in m must be there with its
// checksum. The history is deleted only after this passes.
func verifyExport(dir string, m Manifest) error {
	for name, want := range m.Files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read the export back: %w", err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("%s does not match %s: the history is not deleted", name, manifestFile)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, manifestFile)); err != nil {
		return fmt.Errorf("read the export back: %w", err)
	}
	return nil
}

// exportStep exports and verifies the history, then says where it is and what it holds.
func exportStep(ctx context.Context, e *Env, svc Service, dir string, now time.Time) error {
	m, err := exportHistory(ctx, svc, dir, now)
	if err != nil {
		return err
	}
	if err := verifyExport(dir, m); err != nil {
		return err
	}
	u := e.UI()
	u.OK(fmt.Sprintf("Exported %d grants and %d rules versions", m.Grants, m.RuleVersions), dir)
	u.Warn("The export contains user names and account ids: keep it where only the people who may read the audit can")
	return nil
}
