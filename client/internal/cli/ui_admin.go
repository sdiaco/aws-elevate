package cli

import (
	"context"
	"path/filepath"
	"time"

	"github.com/sdiaco/aws-elevate/client/internal/api"
	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/ui"
)

// uiAdmin gives the Rules view the admin's own AWS calls: the reads of §7.5, with the
// permission sets cached per profile, and the publication on this installation.
type uiAdmin struct {
	aws   AdminAWS
	inst  *api.Installation
	cache *awsx.PermissionSetCache // nil: no cache directory, read AWS every time
}

func newUIAdmin(e *Env, profile string, aws AdminAWS, inst *api.Installation) *uiAdmin {
	a := &uiAdmin{aws: aws, inst: inst}
	if e.CacheDir != "" {
		a.cache = &awsx.PermissionSetCache{
			Path:  filepath.Join(e.CacheDir, profile, "permission-sets.json"),
			Now:   time.Now,
			Fetch: aws.PermissionSets,
		}
	}
	return a
}

func (a *uiAdmin) PermissionSets(ctx context.Context, refresh bool) ([]awsx.Named, error) {
	if a.cache == nil {
		return a.aws.PermissionSets(ctx)
	}
	return a.cache.Get(ctx, refresh)
}

func (a *uiAdmin) Accounts(ctx context.Context) ([]awsx.Named, error) { return a.aws.Accounts(ctx) }

func (a *uiAdmin) Groups(ctx context.Context) ([]awsx.Named, error) { return a.aws.Groups(ctx) }

func (a *uiAdmin) PublishRules(ctx context.Context, latest *int, doc []byte) (int, error) {
	return a.aws.PublishRules(ctx, a.inst.RulesApplicationID, a.inst.RulesProfileID, latest, doc)
}

func (a *uiAdmin) PreviewUpdate(ctx context.Context, v string) (*ui.UpdatePreview, error) {
	p, cmd, err := previewInfra(ctx, a.aws, a.inst.StackID, v)
	if err != nil {
		return nil, err
	}
	return &ui.UpdatePreview{NoChanges: p.NoChanges, Changes: p.Changes, Command: cmd}, nil
}
