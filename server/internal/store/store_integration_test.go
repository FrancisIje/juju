//go:build integration

// Integration tests run against a real Postgres:
//
//	go test -tags=integration ./...
//
// If JUJU_TEST_DATABASE_URL is set (CI, docker compose) that database is
// used. Otherwise an embedded Postgres is started on port 54329, so the tests
// work on a machine without Docker.
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"juju/internal/db"
	"juju/internal/event"
	"juju/internal/fingerprint"
	"juju/internal/store"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx := context.Background()
	url := os.Getenv("JUJU_TEST_DATABASE_URL")
	if url == "" {
		dir, err := os.MkdirTemp("", "juju-pg-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V17).Port(54329).
			RuntimePath(filepath.Join(dir, "runtime")).DataPath(filepath.Join(dir, "data")).
			Logger(nil))
		if err := pg.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "start embedded postgres:", err)
			return 1
		}
		defer pg.Stop() // deferred calls run before os.Exit, because runTests returns first
		url = "postgres://postgres:postgres@localhost:54329/postgres?sslmode=disable"
	}

	var err error
	if pool, err = db.Open(ctx, url); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	return m.Run()
}

// newProject gives each test its own project, so tests never see each
// other's rows and can run in any order.
func newProject(t *testing.T) (*store.Store, store.Project) {
	t.Helper()
	s := store.New(pool)
	p, err := s.CreateProject(context.Background(), "org-"+t.Name(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func newEvent(projectID int64, typ string, line int) *event.Event {
	raw, _ := json.Marshal(map[string]any{
		"event_id": uuid.NewString(), "type": "error", "platform": "dart",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"exception": map[string]any{"type": typ, "message": "boom", "stacktrace": map[string]any{"frames": []any{
			map[string]any{"function": "CartPage.build", "filename": "package:shop/cart.dart", "lineno": line, "in_app": true},
		}}},
	})
	e, err := event.Normalize(raw, time.Now())
	if err != nil {
		panic(err)
	}
	e.ProjectID = projectID
	return e
}

func save(t *testing.T, s *store.Store, e *event.Event) bool {
	t.Helper()
	return saveResult(t, s, e).Inserted
}

func saveResult(t *testing.T, s *store.Store, e *event.Event) store.SaveResult {
	t.Helper()
	res, err := s.SaveEvent(context.Background(), e, fingerprint.Compute(e))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func issues(t *testing.T, s *store.Store, projectID int64) []store.Issue {
	t.Helper()
	is, err := s.ListIssues(context.Background(), projectID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return is
}

func TestDuplicateEventCountsOnce(t *testing.T) {
	s, p := newProject(t)
	e := newEvent(p.ID, "StateError", 88)

	if !save(t, s, e) {
		t.Fatal("first save should insert")
	}
	if save(t, s, e) { // the SDK retried the same event
		t.Fatal("second save of the same event_id should be a no-op")
	}
	is := issues(t, s, p.ID)
	if len(is) != 1 || is[0].EventCount != 1 {
		t.Fatalf("want 1 issue with count 1, got %+v", is)
	}
}

func TestGroupingIntoIssues(t *testing.T) {
	s, p := newProject(t)
	save(t, s, newEvent(p.ID, "StateError", 88))
	save(t, s, newEvent(p.ID, "StateError", 91)) // same bug, line moved
	save(t, s, newEvent(p.ID, "RangeError", 88)) // different bug

	counts := map[string]int64{}
	for _, is := range issues(t, s, p.ID) {
		counts[is.Title] = is.EventCount
	}
	if len(counts) != 2 || counts["StateError: boom"] != 2 || counts["RangeError: boom"] != 1 {
		t.Fatalf("issue counts = %v", counts)
	}
}

func TestFirstAndLastSeen(t *testing.T) {
	s, p := newProject(t)
	early, late := newEvent(p.ID, "E", 1), newEvent(p.ID, "E", 1)
	early.Time = time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	late.Time = time.Now().UTC().Truncate(time.Microsecond)
	save(t, s, late) // arrives first: offline phones send late
	save(t, s, early)

	is := issues(t, s, p.ID)[0]
	if !is.FirstSeen.Equal(early.Time) || !is.LastSeen.Equal(late.Time) {
		t.Fatalf("first_seen %v last_seen %v, want %v / %v", is.FirstSeen, is.LastSeen, early.Time, late.Time)
	}
}

// Many workers hitting the same brand-new bug at once must still produce
// exactly one issue with the right count. This is what the UNIQUE constraint
// plus ON CONFLICT protects.
func TestConcurrentSavesSameIssue(t *testing.T) {
	s, p := newProject(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { save(t, s, newEvent(p.ID, "StateError", 88)) })
	}
	wg.Wait()
	is := issues(t, s, p.ID)
	if len(is) != 1 || is[0].EventCount != 20 {
		t.Fatalf("want 1 issue with 20 events, got %+v", is)
	}
}

func TestProjectsAreIsolated(t *testing.T) {
	s, a := newProject(t)
	b, err := s.CreateProject(context.Background(), "org-"+t.Name(), "other")
	if err != nil {
		t.Fatal(err)
	}
	e := newEvent(a.ID, "StateError", 88)
	save(t, s, e)
	issueID := issues(t, s, a.ID)[0].ID

	// Project b asking for project a's issue id must get nothing.
	evs, err := s.ListIssueEvents(context.Background(), b.ID, issueID, 50)
	if err != nil || len(evs) != 0 {
		t.Fatalf("project b read %d of project a's events (err %v)", len(evs), err)
	}
	evs, _ = s.ListIssueEvents(context.Background(), a.ID, issueID, 50)
	if len(evs) != 1 {
		t.Fatalf("project a should see its own event, got %d", len(evs))
	}
}

func TestProjectKeys(t *testing.T) {
	s, p := newProject(t)
	ctx := context.Background()
	if got, _ := s.ProjectPublicKey(ctx, p.ID); got != p.PublicKey {
		t.Errorf("public key = %q, want %q", got, p.PublicKey)
	}
	hash, _ := s.ProjectSecretHash(ctx, p.ID)
	if string(hash) != string(store.HashSecret(p.SecretKey)) {
		t.Error("stored hash does not match the secret key")
	}
	if _, err := s.ProjectPublicKey(ctx, 999_999); err != store.ErrNotFound {
		t.Errorf("unknown project: err = %v, want ErrNotFound", err)
	}
}

func TestDebugFiles(t *testing.T) {
	s, p := newProject(t)
	ctx := context.Background()
	if _, err := s.FindDebugFile(ctx, p.ID, "sourcemap", "web@1", "app.min.js"); err != store.ErrNotFound {
		t.Fatalf("before upload: err = %v, want ErrNotFound", err)
	}
	if _, err := s.SaveDebugFile(ctx, p.ID, store.DebugFile{Kind: "sourcemap", Release: "web@1", Name: "app.min.js", SHA256: "aaa", Size: 1}); err != nil {
		t.Fatal(err)
	}
	// Uploading the same name again for the release replaces it.
	if _, err := s.SaveDebugFile(ctx, p.ID, store.DebugFile{Kind: "sourcemap", Release: "web@1", Name: "app.min.js", SHA256: "bbb", Size: 2}); err != nil {
		t.Fatal(err)
	}
	sum, err := s.FindDebugFile(ctx, p.ID, "sourcemap", "web@1", "app.min.js")
	if err != nil || sum != "bbb" {
		t.Fatalf("sum = %q, err = %v; want the newer upload", sum, err)
	}
	if _, err := s.FindDebugFile(ctx, p.ID, "sourcemap", "web@2", "app.min.js"); err != store.ErrNotFound {
		t.Fatal("another release must not see this map")
	}
	files, _ := s.ListDebugFiles(ctx, p.ID, "")
	if len(files) != 1 {
		t.Fatalf("listed %d files, want 1", len(files))
	}
	// Another project can't see it.
	other, _ := s.CreateProject(ctx, "org-"+t.Name(), "other")
	if _, err := s.FindDebugFile(ctx, other.ID, "sourcemap", "web@1", "app.min.js"); err != store.ErrNotFound {
		t.Fatal("debug files must be per project")
	}
}

func TestNewIssueAndRegression(t *testing.T) {
	s, p := newProject(t)
	ctx := context.Background()
	first := saveResult(t, s, newEvent(p.ID, "StateError", 88))
	if !first.NewIssue || first.Regressed {
		t.Fatalf("first event: %+v, want a new issue", first)
	}
	if again := saveResult(t, s, newEvent(p.ID, "StateError", 88)); again.NewIssue || again.Regressed {
		t.Fatalf("second event of an open issue: %+v, want neither", again)
	}
	if retry := saveResult(t, s, func() *event.Event { e := newEvent(p.ID, "StateError", 88); return e }()); retry.NewIssue {
		t.Fatal("not new")
	}

	if n, err := s.SetIssueStatus(ctx, p.ID, []int64{first.IssueID}, "resolved", 0); err != nil || n != 1 {
		t.Fatalf("resolve: %d, %v", n, err)
	}
	back := saveResult(t, s, newEvent(p.ID, "StateError", 88))
	if !back.Regressed {
		t.Fatalf("event after resolving: %+v, want a regression", back)
	}
	d, err := s.IssueDetail(ctx, p.ID, first.IssueID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "unresolved" || !d.Regressed {
		t.Errorf("status = %s regressed = %v, want unresolved and regressed", d.Status, d.Regressed)
	}
	// Reopening by hand is not a regression.
	s.SetIssueStatus(ctx, p.ID, []int64{first.IssueID}, "resolved", 0)
	s.SetIssueStatus(ctx, p.ID, []int64{first.IssueID}, "unresolved", 0)
	if d2, _ := s.IssueDetail(ctx, p.ID, first.IssueID); d2.Regressed {
		t.Error("a manual reopen was labelled a regression")
	}
	var kinds []string
	for _, a := range d.Activity {
		kinds = append(kinds, a.Kind)
	}
	if strings.Join(kinds[len(kinds)-3:], ",") != "regressed,resolved,created" {
		t.Errorf("activity = %v", kinds)
	}
}

// Ten workers saving events for one resolved issue at the same moment: the
// issue must regress exactly once, or the team gets ten alerts.
func TestRegressionHappensOnceUnderConcurrency(t *testing.T) {
	s, p := newProject(t)
	ctx := context.Background()
	first := saveResult(t, s, newEvent(p.ID, "RaceError", 1))
	if _, err := s.SetIssueStatus(ctx, p.ID, []int64{first.IssueID}, "resolved", 0); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	regressions := 0
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if saveResult(t, s, newEvent(p.ID, "RaceError", 1)).Regressed {
				mu.Lock()
				regressions++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if regressions != 1 {
		t.Fatalf("%d regressions, want exactly 1", regressions)
	}
}

func TestAffectedUsersAreCountedOnce(t *testing.T) {
	s, p := newProject(t)
	for _, id := range []string{"u1", "u2", "u1", "", "u2"} {
		e := newEvent(p.ID, "UserError", 1)
		if id != "" {
			e.User = &event.User{ID: id}
		}
		save(t, s, e)
	}
	list, _, err := s.SearchIssues(context.Background(), p.ID, store.IssueFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if list[0].EventCount != 5 || list[0].UserCount != 2 {
		t.Fatalf("events %d users %d, want 5 and 2", list[0].EventCount, list[0].UserCount)
	}
	if sum(list[0].Trend) != 5 || list[0].Trend[23] != 5 {
		t.Errorf("trend = %v, want all 5 in the latest hour", list[0].Trend)
	}
}

func TestSearchIssues(t *testing.T) {
	s, p := newProject(t)
	ctx := context.Background()
	a := saveResult(t, s, newEvent(p.ID, "TypeError", 1))
	saveResult(t, s, newEvent(p.ID, "RangeError", 1))
	saveResult(t, s, newEvent(p.ID, "RangeError", 1))
	if _, err := s.SetIssueStatus(ctx, p.ID, []int64{a.IssueID}, "ignored", 0); err != nil {
		t.Fatal(err)
	}
	titles := func(f store.IssueFilter) []string {
		f.Limit = 10
		list, _, err := s.SearchIssues(ctx, p.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range list {
			out = append(out, strings.SplitN(x.Title, ":", 2)[0])
		}
		return out
	}
	if got := titles(store.IssueFilter{Status: "unresolved"}); strings.Join(got, ",") != "RangeError" {
		t.Errorf("unresolved = %v", got)
	}
	if got := titles(store.IssueFilter{Status: "ignored"}); strings.Join(got, ",") != "TypeError" {
		t.Errorf("ignored = %v", got)
	}
	if got := titles(store.IssueFilter{Query: "range"}); strings.Join(got, ",") != "RangeError" {
		t.Errorf("search = %v", got)
	}
	if got := titles(store.IssueFilter{Query: "%"}); len(got) != 0 {
		t.Errorf("%% must be a literal, not a wildcard: %v", got)
	}
	if got := titles(store.IssueFilter{Sort: "events"}); got[0] != "RangeError" {
		t.Errorf("sort by events = %v", got)
	}
	c, _ := s.CountIssues(ctx, p.ID)
	if c.Unresolved != 1 || c.Ignored != 1 {
		t.Errorf("counts = %+v", c)
	}
}

func TestMembershipIsolatesProjects(t *testing.T) {
	s, p := newProject(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, strings.ToLower(t.Name())+"@example.com", "Ada", "hash", "org-"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if role, err := s.MemberRole(ctx, u.ID, p.ID); err != nil || role != "owner" {
		t.Fatalf("own project: %q, %v", role, err)
	}
	other, _ := s.CreateProject(ctx, "someone-else-"+t.Name(), "theirs")
	if _, err := s.MemberRole(ctx, u.ID, other.ID); err != store.ErrNotFound {
		t.Fatalf("someone else's project: err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateUser(ctx, strings.ToLower(t.Name())+"@example.com", "", "h", ""); err != store.ErrConflict {
		t.Fatalf("duplicate email: %v", err)
	}
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// Up, down, up proves the down migration really works, so a bad deploy can
// be rolled back. Runs last-ish by name but resets to "up" either way.
func TestMigrationsRoundTrip(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("JUJU_TEST_DATABASE_URL") != "" {
		t.Skip("drops all tables; only run against the throwaway embedded database")
	}
	for _, cmd := range []string{"down", "up"} {
		if err := db.Migrate(ctx, pool, cmd); err != nil {
			t.Fatalf("migrate %s: %v", cmd, err)
		}
	}
	if _, err := store.New(pool).CreateProject(ctx, "after-roundtrip", "p"); err != nil {
		t.Fatalf("schema unusable after down+up: %v", err)
	}
}
