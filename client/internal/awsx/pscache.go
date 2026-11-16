package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// PermissionSetCacheTTL is how long the list of permission sets is reused (design §7.5).
const PermissionSetCacheTTL = 10 * time.Minute

// PermissionSetCache keeps the list of permission sets (names and ARNs only) in a file of
// the admin's computer, one per profile.
type PermissionSetCache struct {
	Path  string
	Now   func() time.Time
	Fetch func(ctx context.Context) ([]Named, error)
}

type cachedSets struct {
	At    time.Time `json:"at"`
	Items []Named   `json:"items"`
}

// Get returns the cached list while it is fresh, otherwise, or with refresh, reads AWS
// and stores the answer.
func (c *PermissionSetCache) Get(ctx context.Context, refresh bool) ([]Named, error) {
	if !refresh {
		data, err := os.ReadFile(c.Path)
		switch {
		case err == nil:
			var cached cachedSets
			if json.Unmarshal(data, &cached) == nil && c.Now().Sub(cached.At) < PermissionSetCacheTTL {
				return cached.Items, nil
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("read the permission set cache: %w", err)
		}
	}
	items, err := c.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(cachedSets{At: c.Now(), Items: items})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return nil, fmt.Errorf("write the permission set cache: %w", err)
	}
	if err := os.WriteFile(c.Path, data, 0o600); err != nil {
		return nil, fmt.Errorf("write the permission set cache: %w", err)
	}
	return items, nil
}
