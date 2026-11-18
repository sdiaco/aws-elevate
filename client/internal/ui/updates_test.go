package ui

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/sdiaco/aws-elevate/client/internal/awsx"
	"github.com/sdiaco/aws-elevate/client/internal/release"
	"github.com/sdiaco/aws-elevate/client/internal/version"
)

func (f *fakeAdmin) PreviewUpdate(_ context.Context, v string) (*UpdatePreview, error) {
	f.previewed = append(f.previewed, v)
	if v == "9.9.9" {
		return &UpdatePreview{NoChanges: true}, nil
	}
	return &UpdatePreview{
		Changes: []awsx.Change{{Action: "Modify", Resource: "ApiFunction", Type: "AWS::Lambda::Function", Replacement: "False"}},
		Command: "aws cloudformation update-stack --stack-name elevate --template-url https://example.com/v" + v + "/elevate.yaml",
	}, nil
}

// updatesHarness: client 0.0.1 on infrastructure 0.0.1, with a compatible client release,
// an infrastructure release that needs a newer client, and a client update to install.
func updatesHarness(checked bool) (*harness, *fakeAdmin, *[]string) {
	h, admin := rulesHarness()
	h.app.Me.InfraVersion = "0.0.1"
	installed := &[]string{}
	h.app.Updates = &Updates{
		Checked:      checked,
		SettingsPath: "/home/me/.config/aws-elevate/config.json",
		Releases: func(context.Context) ([]release.Release, error) {
			return []release.Release{
				{Version: "0.0.2", Type: "fix", Name: "Retry fix", URL: "https://example.com/r/0.0.2", MinInfra: "0.0.1", MinClient: "0.0.1"},
				{Version: "0.1.0", Type: "security", Name: "Tighter API", URL: "https://example.com/r/0.1.0", MinInfra: "0.1.0", MinClient: "0.1.0"},
			}, nil
		},
		InstallClient: func(_ context.Context, v string) (string, error) {
			*installed = append(*installed, v)
			return "Updated /usr/local/bin/aws-elevate from 0.0.1 to " + v + ".", nil
		},
	}
	return h, admin, installed
}

func TestUpdatesIsForAdminsOnly(t *testing.T) {
	for _, role := range []string{"requester", "approver", "auditor"} {
		h, _, _ := updatesHarness(true)
		h.app.Me.Roles = []string{role}
		if w := h.do(t, "GET", "/updates", nil); w.Code != 404 {
			t.Errorf("/updates as %s: %d, want 404", role, w.Code)
		}
	}
	h, _, _ := updatesHarness(true)
	contains(t, h.get(t, "/requests"), `href="/updates"`)
}

func TestUpdatesListClientAndInfrastructureReleases(t *testing.T) {
	if version.Client != "0.0.1" {
		t.Skip("the test expects the development version 0.0.1")
	}
	h, _, _ := updatesHarness(true)
	out := h.get(t, "/updates")
	contains(t, out,
		"Client 0.0.1", "infrastructure 0.0.1",
		"0.0.2", "Retry fix", "https://example.com/r/0.0.2", "Fix",
		`action="/updates/client"`, "Update client to 0.0.2",
		"0.1.0", "Tighter API", "Security", "needs client 0.1.0",
		`action="/updates/preview"`, `name="version" value="0.1.0"`,
	)
}

func TestUpdatesSaysWhenTheCheckIsOff(t *testing.T) {
	h, _, _ := updatesHarness(false)
	out := h.get(t, "/updates")
	contains(t, out, "turned off", "checkUpdates: false", "/home/me/.config/aws-elevate/config.json")
	lacks(t, out, "Update client to")
}

func TestUpdateClientInstallsTheNewestCompatibleRelease(t *testing.T) {
	h, _, installed := updatesHarness(true)
	w := h.do(t, "POST", "/updates/client", url.Values{})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(*installed) != 1 || (*installed)[0] != "0.0.2" {
		t.Fatalf("installed %v, want [0.0.2]", *installed)
	}
	contains(t, w.Body.String(), "Updated /usr/local/bin/aws-elevate from 0.0.1 to 0.0.2.", "aws elevate ui")
}

func TestUpdateClientShowsTheFailure(t *testing.T) {
	h, _, _ := updatesHarness(true)
	h.app.Updates.InstallClient = func(context.Context, string) (string, error) {
		return "", errors.New("cannot replace /usr/local/bin/aws-elevate: install it with sudo")
	}
	w := h.do(t, "POST", "/updates/client", url.Values{})
	contains(t, w.Body.String(), "install it with sudo")
}

func TestPreviewShowsChangesAndTheCommandsWithoutApplying(t *testing.T) {
	h, admin, _ := updatesHarness(true)
	w := h.do(t, "POST", "/updates/preview", url.Values{"version": {"0.1.0"}})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(admin.previewed) != 1 || admin.previewed[0] != "0.1.0" {
		t.Fatalf("previewed %v", admin.previewed)
	}
	contains(t, w.Body.String(),
		"Modify", "ApiFunction", "AWS::Lambda::Function",
		"nothing was changed",
		"Show equivalent command", "aws elevate admin updates --preview 0.1.0",
		"aws cloudformation update-stack", "https://example.com/v0.1.0/elevate.yaml",
		"your own admin role",
	)
}

func TestPreviewWithoutChanges(t *testing.T) {
	h, _, _ := updatesHarness(true)
	contains(t, h.do(t, "POST", "/updates/preview", url.Values{"version": {"9.9.9"}}).Body.String(), "No changes")
}

func TestPreviewRefusesAMalformedVersion(t *testing.T) {
	h, admin, _ := updatesHarness(true)
	w := h.do(t, "POST", "/updates/preview", url.Values{"version": {"1.0.0/../../x"}})
	if w.Code != 400 || len(admin.previewed) != 0 {
		t.Fatalf("status %d, previewed %v", w.Code, admin.previewed)
	}
}

func TestUpdatesWithoutAnyReleaseSaysSoCalmly(t *testing.T) {
	h, _, _ := updatesHarness(true)
	h.app.Updates.Releases = func(context.Context) ([]release.Release, error) { return nil, nil }
	out, _, _ := strings.Cut(h.get(t, "/updates"), `<template id="closed-alert">`)
	contains(t, out, `class="alert info"`, "No release published yet.", "Client ")
	lacks(t, out, `class="alert error"`, "is up to date", "newest that works")
}

func TestUpdatesKeepsTheInstalledVersionsWhenTheReleasesCannotBeRead(t *testing.T) {
	h, _, _ := updatesHarness(true)
	h.app.Updates.Releases = func(context.Context) ([]release.Release, error) {
		return nil, errors.New("read the releases: 404: the repository is not reachable")
	}
	out := h.get(t, "/updates")
	contains(t, out, `class="alert error"`, "the repository is not reachable", "Client ", "infrastructure ")
}
