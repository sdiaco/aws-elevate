package ui

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// Updates is what the Updates view needs from the process (design §12.3): the releases
// and the client update, the same ones `aws elevate admin updates` and `aws-elevate
// update` use.
type Updates struct {
	Checked      bool   // false: checkUpdates is off in the settings
	SettingsPath string // where the user turns the check back on
	Releases     func(ctx context.Context) ([]release.Release, error)
	// InstallClient replaces this executable with release v and says what it did.
	InstallClient func(ctx context.Context, v string) (string, error)
}

// UpdatePreview is an infrastructure update seen through a CloudFormation change set: it
// changes nothing. Command applies the update with the admin's own role of the account.
type UpdatePreview struct {
	NoChanges bool
	Changes   []awsx.Change
	Command   string
}

var typeLabels = map[string]string{"security": "Security", "fix": "Fix", "feature": "Feature"}

type releaseRow struct {
	release.Release
	TypeLabel  string
	NeedClient string // the infrastructure release needs a newer client than this one
}

type updatesView struct {
	Off          bool
	SettingsPath string
	Client       string
	Infra        string
	Clients      []releaseRow
	Infras       []releaseRow
}

// rows labels releases; for infrastructure ones it also says when this client is too old.
func rows(rels []release.Release, infra bool) []releaseRow {
	out := make([]releaseRow, len(rels))
	for i, r := range rels {
		out[i] = releaseRow{Release: r, TypeLabel: typeLabels[r.Type]}
		if c, err := version.Cmp(version.Client, r.MinClient); infra && (err != nil || c < 0) {
			out[i].NeedClient = r.MinClient
		}
	}
	return out
}

func (a *App) updatesPage(w http.ResponseWriter, r *http.Request) {
	v := updatesView{Client: version.Client, Infra: a.me().InfraVersion, SettingsPath: a.Updates.SettingsPath}
	if !a.Updates.Checked {
		v.Off = true
		a.show(w, http.StatusOK, "updates", "Updates", "/updates", v)
		return
	}
	rels, err := a.Updates.Releases(r.Context())
	if err != nil {
		a.fail(w, "Updates", "/updates", err)
		return
	}
	v.Clients = rows(release.ClientUpdates(rels, version.Client, a.me().InfraVersion), false)
	v.Infras = rows(release.InfraUpdates(rels, a.me().InfraVersion), true)
	a.show(w, http.StatusOK, "updates", "Updates", "/updates", v)
}

// updateClient installs the newest client release that works with the infrastructure.
func (a *App) updateClient(w http.ResponseWriter, r *http.Request) {
	if !a.Updates.Checked {
		a.show(w, http.StatusConflict, "message", "Updates", "/updates", "Updates",
			Alert{Kind: "error", Text: "The update check is turned off (checkUpdates: false in " + a.Updates.SettingsPath + ")."})
		return
	}
	rels, err := a.Updates.Releases(r.Context())
	if err != nil {
		a.fail(w, "Updates", "/updates", err)
		return
	}
	next := release.ClientUpdates(rels, version.Client, a.me().InfraVersion)
	if len(next) == 0 {
		a.show(w, http.StatusOK, "message", "Updates", "/updates", "Updates",
			Alert{Kind: "info", Text: "This client is the newest that works with infrastructure " + a.me().InfraVersion + "."})
		return
	}
	msg, err := a.Updates.InstallClient(r.Context(), next[0].Version)
	if err != nil {
		a.fail(w, "Updates", "/updates", err)
		return
	}
	a.show(w, http.StatusOK, "message", "Updates", "/updates", "Updates",
		Alert{Kind: "success", Text: msg + " Stop this interface and start aws elevate ui again to use it."})
}

type previewView struct {
	Version string
	*UpdatePreview
}

// preview creates a change set of the stack for an infrastructure release: CloudFormation
// lists what would change and applies nothing (design §12.3).
func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	v := r.PostFormValue("version")
	if _, err := version.Cmp(v, v); err != nil {
		a.show(w, http.StatusBadRequest, "message", "Updates", "/updates", "Updates",
			Alert{Kind: "error", Text: fmt.Sprintf("%q is not a version.", v)})
		return
	}
	p, err := a.Admin.PreviewUpdate(r.Context(), v)
	if err != nil {
		a.fail(w, "Updates", "/updates", err)
		return
	}
	a.show(w, http.StatusOK, "preview", "Infrastructure "+v, "/updates", previewView{Version: v, UpdatePreview: p})
}
