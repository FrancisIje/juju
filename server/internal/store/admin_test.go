package store

import (
	"testing"
	"time"
)

func TestProjectStatus(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	quota := func(n int64) *int64 { return &n }
	cases := []struct {
		name string
		p    AdminProject
		want string
	}{
		{"never sent anything", AdminProject{}, StatusNoEvents},
		{"sent an event just now", AdminProject{LastEventAt: ago(time.Minute), EventsAllTime: 1}, StatusActive},
		{"exactly at the quiet limit is still active", AdminProject{LastEventAt: ago(QuietAfter), EventsAllTime: 1}, StatusActive},
		{"past the quiet limit", AdminProject{LastEventAt: ago(QuietAfter + time.Second), EventsAllTime: 1}, StatusQuiet},
		// Counted before last_accepted_at existed: it has sent events, we just don't know when.
		{"counted events but no known time", AdminProject{EventsAllTime: 5}, StatusQuiet},
		{"below 80 % of quota", AdminProject{LastEventAt: ago(time.Minute), EventsThisMonth: 79, MonthlyQuota: quota(100)}, StatusActive},
		{"at 80 % of quota", AdminProject{LastEventAt: ago(time.Minute), EventsThisMonth: 80, MonthlyQuota: quota(100)}, StatusNearQuota},
		{"just under quota", AdminProject{EventsThisMonth: 99, MonthlyQuota: quota(100)}, StatusNearQuota},
		{"at quota: ingest is refusing events", AdminProject{EventsThisMonth: 100, MonthlyQuota: quota(100)}, StatusOverQuota},
		// A batch can pass the quota by up to 100 events.
		{"past quota", AdminProject{EventsThisMonth: 150, MonthlyQuota: quota(100)}, StatusOverQuota},
		// Ingest refuses everything with a quota of 0, so this isn't "no events".
		{"quota of zero", AdminProject{MonthlyQuota: quota(0)}, StatusOverQuota},
		{"quota outranks quiet", AdminProject{LastEventAt: ago(30 * 24 * time.Hour), EventsThisMonth: 100, MonthlyQuota: quota(100)}, StatusOverQuota},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectStatus(tc.p, now); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOrgStatus(t *testing.T) {
	with := func(statuses ...string) []AdminProject {
		var out []AdminProject
		for _, s := range statuses {
			out = append(out, AdminProject{Status: s})
		}
		return out
	}
	cases := []struct {
		name     string
		projects []AdminProject
		want     string
	}{
		{"no projects", nil, StatusNoProjects},
		{"nothing sent", with(StatusNoEvents, StatusNoEvents), StatusNoEvents},
		{"one quiet project is more than none", with(StatusNoEvents, StatusQuiet), StatusQuiet},
		{"one active project makes it active", with(StatusQuiet, StatusActive), StatusActive},
		{"a project at its quota is still receiving", with(StatusQuiet, StatusOverQuota), StatusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orgStatus(tc.projects); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// Numbers must land on the right organization: a mix-up here would show one
// customer another's usage.
func TestSummarize(t *testing.T) {
	early := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(48 * time.Hour)
	orgs := []AdminOrg{{ID: 1}, {ID: 2}, {ID: 3}}
	got := summarize(orgs, []AdminProject{
		{OrgID: 1, Status: StatusActive, EventsThisMonth: 10, EventsLastMonth: 7, Errors24h: 2, Unresolved: 3, LastEventAt: &early},
		{OrgID: 1, Status: StatusQuiet, EventsThisMonth: 5, Errors24h: 1, Unresolved: 1, LastEventAt: &late},
		{OrgID: 2, Status: StatusNoEvents},
	})
	if got.Organizations != 3 || got.Projects != 3 || got.EventsThisMonth != 15 || got.EventsLastMonth != 7 || got.Errors24h != 3 || got.Unresolved != 4 {
		t.Errorf("totals = %+v", got)
	}
	if got.ByStatus[StatusActive] != 1 || got.ByStatus[StatusQuiet] != 1 || got.ByStatus[StatusNoEvents] != 1 {
		t.Errorf("by status = %v", got.ByStatus)
	}
	if o := orgs[0]; o.Projects != 2 || o.EventsThisMonth != 15 || o.Errors24h != 3 || o.Unresolved != 4 || o.Status != StatusActive || !o.LastEventAt.Equal(late) {
		t.Errorf("org 1 = %+v", o)
	}
	if o := orgs[1]; o.Projects != 1 || o.EventsThisMonth != 0 || o.Status != StatusNoEvents || o.LastEventAt != nil {
		t.Errorf("org 2 = %+v", o)
	}
	if o := orgs[2]; o.Projects != 0 || o.Status != StatusNoProjects {
		t.Errorf("org 3 = %+v", o)
	}
}
