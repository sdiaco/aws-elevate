// Package release reads the releases of aws-elevate from the public repository (design
// §12.3) and tells which ones are updates for this client and its infrastructure.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/sdiaco/aws-elevate/client/internal/version"
)

// DefaultURL lists the releases, newest first. One page of 100 covers years of releases.
const DefaultURL = "https://api.github.com/repos/sdiaco/aws-elevate/releases?per_page=100"

// Release is one published version of client and infrastructure. Its notes start with
// header lines written by the release script:
//
//	type: security | fix | feature
//	minInfra: <oldest infrastructure the client of this release works with>
//	minClient: <oldest client the infrastructure of this release accepts>
type Release struct {
	Version   string
	Type      string
	Name      string
	URL       string
	MinInfra  string
	MinClient string
}

// DefaultDownloadURL holds the files of each release under v<version>/.
const DefaultDownloadURL = "https://github.com/sdiaco/aws-elevate/releases/download"

// Lister reads the release list and downloads the files of a release.
type Lister struct {
	HTTP        *http.Client
	URL         string
	DownloadURL string
}

func New() *Lister {
	return &Lister{HTTP: http.DefaultClient, URL: DefaultURL, DownloadURL: DefaultDownloadURL}
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// githubMessage is the "message" of a GitHub error body, or the body when it has none.
func githubMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(body))
}

// List returns the published releases, newest first, without drafts and prereleases.
func (l *Lister) List(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read the releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// GitHub answers 404 for a repository it does not show: private, renamed or gone.
		return nil, fmt.Errorf("read the releases: GitHub answered 404: the repository is not reachable or has no published release yet")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("read the releases: GitHub answered %d: %s", resp.StatusCode, githubMessage(body))
	}
	var all []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, fmt.Errorf("read the releases: %w", err)
	}
	var out []Release
	for _, g := range all {
		v := strings.TrimPrefix(g.TagName, "v")
		if g.Draft || g.Prerelease {
			continue
		}
		if _, err := version.Cmp(v, v); err != nil {
			continue
		}
		r := Release{Version: v, Name: g.Name, URL: g.HTMLURL}
		header(g.Body, &r)
		out = append(out, r)
	}
	return out, nil
}

// header reads the "key: value" lines at the top of the notes, up to the first other line.
func header(body string, r *Release) {
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			return
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "type":
			r.Type = value
		case "minInfra":
			r.MinInfra = value
		case "minClient":
			r.MinClient = value
		default:
			return
		}
	}
}

// ClientUpdates are the releases newer than client whose client works with infra, newest
// first. A release that does not state its minInfra is not proposed.
func ClientUpdates(rels []Release, client, infra string) []Release {
	var out []Release
	for _, r := range rels {
		if newer(r.Version, client) && atLeast(infra, r.MinInfra) {
			out = append(out, r)
		}
	}
	return newestFirst(out)
}

// InfraUpdates are the releases newer than infra, newest first.
func InfraUpdates(rels []Release, infra string) []Release {
	var out []Release
	for _, r := range rels {
		if newer(r.Version, infra) {
			out = append(out, r)
		}
	}
	return newestFirst(out)
}

// newestFirst sorts by version: GitHub lists by date, and a patch of an older line can
// be published after a newer release. Versions here are already valid.
func newestFirst(rels []Release) []Release {
	slices.SortStableFunc(rels, func(a, b Release) int {
		c, _ := version.Cmp(b.Version, a.Version)
		return c
	})
	return rels
}

func newer(v, than string) bool {
	c, err := version.Cmp(v, than)
	return err == nil && c > 0
}

func atLeast(v, min string) bool {
	c, err := version.Cmp(v, min)
	return err == nil && c >= 0
}
