package awsx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPermissionSetCacheIsReusedForTenMinutes(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	reads := 0
	c := &PermissionSetCache{
		Path: filepath.Join(t.TempDir(), "work", "permission-sets.json"),
		Now:  func() time.Time { return now },
		Fetch: func(context.Context) ([]Named, error) {
			reads++
			return []Named{{ID: "arn:aws:sso:::permissionSet/ssoins-1/ps-1", Name: "ssm-write"}}, nil
		},
	}
	ctx := context.Background()
	for _, step := range []struct {
		after   time.Duration
		refresh bool
		reads   int
	}{
		{0, false, 1},
		{9 * time.Minute, false, 1},
		{9 * time.Minute, true, 2}, // "refresh" reads AWS again
		{20 * time.Minute, false, 3},
	} {
		now = now.Add(step.after)
		items, err := c.Get(ctx, step.refresh)
		if err != nil || len(items) != 1 || items[0].Name != "ssm-write" || reads != step.reads {
			t.Fatalf("after %s refresh=%v: %v %v reads=%d", step.after, step.refresh, items, err, reads)
		}
	}
	info, err := os.Stat(c.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cache file %v %v", info, err)
	}
}
