//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"juju/internal/store"
)

func TestReleaseCommits(t *testing.T) {
	ctx := context.Background()
	s, p := newProject(t)
	e := newEvent(p.ID, "StateError", 10)
	e.Release = "shop@1.2.0"
	res := saveResult(t, s, e)

	if _, err := s.ReleaseInfo(ctx, p.ID, "shop@1.2.0"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before CI records it: %v, want ErrNotFound", err)
	}
	if _, err := s.SaveRelease(ctx, p.ID, store.Release{Name: "shop@1.2.0", Commit: "aaaaaaa", Repository: "https://github.com/acme/shop"}); err != nil {
		t.Fatal(err)
	}
	// CI runs again for the same release (a re-run job): it overwrites.
	if _, err := s.SaveRelease(ctx, p.ID, store.Release{Name: "shop@1.2.0", Commit: "bbbbbbb", Repository: "https://github.com/acme/shop"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.ReleaseInfo(ctx, p.ID, "shop@1.2.0")
	if err != nil || r.Commit != "bbbbbbb" {
		t.Fatalf("release = %+v, %v", r, err)
	}

	d, err := s.IssueDetail(ctx, p.ID, res.IssueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Commits) != 1 || d.Commits[0].Name != "shop@1.2.0" || d.Commits[0].Commit != "bbbbbbb" {
		t.Errorf("issue commits = %+v", d.Commits)
	}

	// Another project with a release of the same name sees only its own.
	s2, p2 := newProject2Store(t)
	if _, err := s2.ReleaseInfo(ctx, p2.ID, "shop@1.2.0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("project %d sees project %d's release: %v", p2.ID, p.ID, err)
	}
}

func TestIssueLinks(t *testing.T) {
	ctx := context.Background()
	s, p := newProject(t)
	res := saveResult(t, s, newEvent(p.ID, "StateError", 10))

	pr := "https://github.com/acme/shop/pull/7"
	if _, err := s.AddIssueLink(ctx, p.ID, res.IssueID, store.IssueLink{URL: pr, Title: "Fix"}); err != nil {
		t.Fatal(err)
	}
	// The same URL again (an agent retrying) updates the title, no duplicate.
	if _, err := s.AddIssueLink(ctx, p.ID, res.IssueID, store.IssueLink{URL: pr, Title: "Fix empty cart"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.IssueDetail(ctx, p.ID, res.IssueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Links) != 1 || d.Links[0].Title != "Fix empty cart" {
		t.Errorf("links = %+v", d.Links)
	}

	// A key for another project can't attach links to this project's issue,
	// even knowing its id.
	_, p2 := newProject2Store(t)
	if _, err := s.AddIssueLink(ctx, p2.ID, res.IssueID, store.IssueLink{URL: "https://evil.example/x"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cross-project link: %v, want ErrNotFound", err)
	}
	if d, _ := s.IssueDetail(ctx, p.ID, res.IssueID); len(d.Links) != 1 {
		t.Errorf("links after cross-project attempt = %+v", d.Links)
	}
}

func TestWebhookSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, p := newProject(t)
	want := store.AlertSettings{EmailTo: []string{}, OnNewIssue: true, WebhookURL: "https://agents.example.com/juju", WebhookSecret: "whsec_1"}
	if err := s.SaveAlertSettings(ctx, p.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.AlertSettings(ctx, p.ID)
	if err != nil || got.WebhookURL != want.WebhookURL || got.WebhookSecret != want.WebhookSecret {
		t.Errorf("settings = %+v, %v", got, err)
	}
}

func newProject2Store(t *testing.T) (*store.Store, store.Project) {
	t.Helper()
	s := store.New(pool)
	p, err := s.CreateProject(context.Background(), "org2-"+t.Name(), "other")
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}
