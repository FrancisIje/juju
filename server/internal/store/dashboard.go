package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queries for the dashboard. Every project-scoped query takes the project
// id it was authorized for, so an issue id from another project returns
// nothing instead of leaking.

var ErrConflict = errors.New("already exists")

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// IsSuperAdmin can see every organization on the server (see package ui's
	// superAdmin). Only `juju user superadmin` sets it.
	IsSuperAdmin bool `json:"is_superadmin,omitempty"`
}

// CountUsers is used to allow the very first sign-up (setting up a new
// server) and refuse later ones unless sign-up is enabled.
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// OrgChoice says what CreateUser does when the organization name exists.
type OrgChoice int

const (
	// NewOrganization fails with ErrOrgTaken if the name exists. Public
	// sign-up must use this: joining by name would let anyone who guesses
	// "Acme" become an owner of Acme and read all its projects.
	NewOrganization OrgChoice = iota
	// JoinOrCreate adds the user to an existing organization of that name.
	// Only for admins (the `juju user create` command).
	JoinOrCreate
)

// ErrOrgTaken: sign-up named an organization that already exists.
var ErrOrgTaken = errors.New("organization name taken")

// CreateUser makes a user and, if org is set, makes them owner of that
// organization; choice says whether an existing one may be joined.
func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash, org string, choice OrgChoice) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)
	u := User{Email: email, Name: name}
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO NOTHING
		RETURNING id, created_at`, email, name, passwordHash).Scan(&u.ID, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrConflict
	}
	if err != nil {
		return User{}, err
	}
	if org != "" {
		var orgID int64
		q := `INSERT INTO organizations (name) VALUES ($1) ON CONFLICT (name) DO NOTHING RETURNING id`
		if choice == JoinOrCreate {
			q = `INSERT INTO organizations (name) VALUES ($1) ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`
		}
		err := tx.QueryRow(ctx, q, org).Scan(&orgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrOrgTaken // the user row rolls back too
		}
		if err != nil {
			return User{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (user_id, organization_id, role) VALUES ($1, $2, 'owner') ON CONFLICT DO NOTHING`, u.ID, orgID); err != nil {
			return User{}, err
		}
	}
	return u, tx.Commit(ctx)
}

// UserByEmail returns the user and their password hash, for login.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, string, error) {
	var u User
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT id, email, name, created_at, is_superadmin, password_hash FROM users WHERE email = $1`, email).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt, &u.IsSuperAdmin, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	return u, hash, err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`, tokenHash, userID, expires)
	return err
}

// SessionUser returns the user for a live session.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, time.Time, error) {
	var u User
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.created_at, u.is_superadmin, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, tokenHash).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt, &u.IsSuperAdmin, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, time.Time{}, ErrNotFound
	}
	return u, expires, err
}

func (s *Store) ExtendSession(ctx context.Context, tokenHash []byte, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET expires_at = $2 WHERE token_hash = $1`, tokenHash, expires)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

type OrgProjects struct {
	ID       int64            `json:"id"`
	Name     string           `json:"name"`
	Role     string           `json:"role"`
	Projects []ProjectSummary `json:"projects"`
}

type ProjectSummary struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Unresolved int64  `json:"unresolved"`
}

// UserOrgs lists the organizations a user belongs to, with their projects.
func (s *Store) UserOrgs(ctx context.Context, userID int64) ([]OrgProjects, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.name, m.role, p.id, p.name,
		       (SELECT count(*) FROM issues i WHERE i.project_id = p.id AND i.status = 'unresolved')
		FROM memberships m
		JOIN organizations o ON o.id = m.organization_id
		LEFT JOIN projects p ON p.organization_id = o.id
		WHERE m.user_id = $1
		ORDER BY o.name, p.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgProjects
	for rows.Next() {
		var orgID int64
		var orgName, role string
		var pid *int64
		var pname *string
		var unresolved int64
		if err := rows.Scan(&orgID, &orgName, &role, &pid, &pname, &unresolved); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != orgID {
			out = append(out, OrgProjects{ID: orgID, Name: orgName, Role: role, Projects: []ProjectSummary{}})
		}
		if pid != nil {
			cur := &out[len(out)-1]
			cur.Projects = append(cur.Projects, ProjectSummary{ID: *pid, Name: *pname, Unresolved: unresolved})
		}
	}
	return out, rows.Err()
}

// MemberRole returns the user's role in the organization that owns a
// project, or ErrNotFound if they aren't a member. This is the one check
// every project page relies on.
func (s *Store) MemberRole(ctx context.Context, userID, projectID int64) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT m.role FROM projects p
		JOIN memberships m ON m.organization_id = p.organization_id AND m.user_id = $1
		WHERE p.id = $2`, userID, projectID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// OrgRole is MemberRole for an organization.
func (s *Store) OrgRole(ctx context.Context, userID, orgID int64) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, `SELECT role FROM memberships WHERE user_id = $1 AND organization_id = $2`, userID, orgID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

// CreateProjectInOrg is CreateProject for an existing organization id.
func (s *Store) CreateProjectInOrg(ctx context.Context, orgID int64, name string) (Project, error) {
	public, err := randomKey("pk_")
	if err != nil {
		return Project{}, err
	}
	secret, err := randomKey("sk_")
	if err != nil {
		return Project{}, err
	}
	p := Project{Name: name, PublicKey: public, SecretKey: secret}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO projects (organization_id, name, public_key, secret_key_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (organization_id, name) DO NOTHING
		RETURNING id, (SELECT name FROM organizations WHERE id = $1)`,
		orgID, name, public, HashSecret(secret)).Scan(&p.ID, &p.Org)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrConflict
	}
	return p, err
}

func (s *Store) ProjectByID(ctx context.Context, id int64) (Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, o.name, p.name, p.public_key FROM projects p
		JOIN organizations o ON o.id = p.organization_id WHERE p.id = $1`, id).
		Scan(&p.ID, &p.Org, &p.Name, &p.PublicKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// RotateSecret replaces a project's secret key. The old one stops working
// immediately; the new one is returned once.
func (s *Store) RotateSecret(ctx context.Context, projectID int64) (string, error) {
	secret, err := randomKey("sk_")
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `UPDATE projects SET secret_key_hash = $2 WHERE id = $1`, projectID, HashSecret(secret))
	return secret, err
}

type IssueRow struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Culprit     string     `json:"culprit"`
	Status      string     `json:"status"`
	Platform    string     `json:"platform"`
	LastRelease string     `json:"last_release"`
	EventCount  int64      `json:"event_count"`
	UserCount   int64      `json:"user_count"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	StatusAt    *time.Time `json:"status_changed_at"`
	// Regressed: unresolved because it happened again after being resolved
	// (not because someone reopened it by hand).
	Regressed bool `json:"regressed"`
	// Events per hour over the last 24 hours, oldest first.
	Trend []int `json:"trend"`
}

type IssueFilter struct {
	Status      string // unresolved | resolved | ignored | "" for all
	Query       string // matches title or culprit
	Environment string
	Sort        string // last_seen | first_seen | events | users
	Limit       int
	Offset      int
}

type IssueCounts struct {
	Unresolved int64 `json:"unresolved"`
	Resolved   int64 `json:"resolved"`
	Ignored    int64 `json:"ignored"`
}

// SearchIssues is the issue list: filtered, sorted, a page at a time, with
// each issue's 24-hour trend.
func (s *Store) SearchIssues(ctx context.Context, projectID int64, f IssueFilter) ([]IssueRow, int64, error) {
	where := []string{"i.project_id = $1"}
	args := []any{projectID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.Status != "" {
		where = append(where, "i.status = "+arg(f.Status))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		// Escape LIKE wildcards so searching for "50%" means the text "50%".
		pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		p := arg(pattern)
		where = append(where, fmt.Sprintf("(i.title ILIKE %s OR i.culprit ILIKE %s)", p, p))
	}
	if f.Environment != "" {
		where = append(where, "EXISTS (SELECT 1 FROM events e WHERE e.issue_id = i.id AND e.environment = "+arg(f.Environment)+")")
	}
	order := map[string]string{
		"first_seen": "i.first_seen DESC",
		"events":     "i.event_count DESC",
		"users":      "i.user_count DESC",
	}[f.Sort]
	if order == "" {
		order = "i.last_seen DESC"
	}
	cond := strings.Join(where, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM issues i WHERE "+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := arg(f.Limit), arg(f.Offset)
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.title, i.culprit, i.status, i.platform, i.last_release, i.event_count, i.user_count,
		       i.first_seen, i.last_seen, i.status_changed_at,
		       coalesce((SELECT a.kind = 'regressed' FROM issue_activity a WHERE a.issue_id = i.id ORDER BY a.created_at DESC, a.id DESC LIMIT 1), false) AND i.status = 'unresolved'
		FROM issues i WHERE `+cond+`
		ORDER BY `+order+`, i.id DESC
		LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IssueRow, error) {
		var x IssueRow
		err := r.Scan(&x.ID, &x.Title, &x.Culprit, &x.Status, &x.Platform, &x.LastRelease, &x.EventCount, &x.UserCount, &x.FirstSeen, &x.LastSeen, &x.StatusAt, &x.Regressed)
		x.Trend = make([]int, 24)
		return x, err
	})
	if err != nil || len(list) == 0 {
		return list, total, err
	}
	return list, total, s.fillTrends(ctx, list)
}

// fillTrends adds hourly event counts for a page of issues in one query.
func (s *Store) fillTrends(ctx context.Context, list []IssueRow) error {
	ids := make([]int64, len(list))
	index := map[int64]int{}
	for i, x := range list {
		ids[i] = x.ID
		index[x.ID] = i
	}
	rows, err := s.pool.Query(ctx, `
		SELECT issue_id, floor(extract(epoch FROM now() - timestamp) / 3600)::int AS hours_ago, count(*)
		FROM events
		WHERE issue_id = ANY($1) AND timestamp > now() - interval '24 hours'
		GROUP BY 1, 2`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ago, n int
		if err := rows.Scan(&id, &ago, &n); err != nil {
			return err
		}
		if ago >= 0 && ago < 24 {
			list[index[id]].Trend[23-ago] += n
		}
	}
	return rows.Err()
}

func (s *Store) CountIssues(ctx context.Context, projectID int64) (IssueCounts, error) {
	var c IssueCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'unresolved'),
		       count(*) FILTER (WHERE status = 'resolved'),
		       count(*) FILTER (WHERE status = 'ignored')
		FROM issues WHERE project_id = $1`, projectID).Scan(&c.Unresolved, &c.Resolved, &c.Ignored)
	return c, err
}

type IssueDetail struct {
	IssueRow
	Fingerprint  string   `json:"fingerprint"`
	Environments []string `json:"environments"`
	Releases     []string `json:"releases"`
	// Commits are the recorded commits of Releases, for those CI recorded.
	Commits  []Release   `json:"commits"`
	Links    []IssueLink `json:"links"`
	Activity []Activity  `json:"activity"`
}

type Activity struct {
	Kind      string    `json:"kind"`
	User      string    `json:"user,omitempty"`
	Release   string    `json:"release,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) IssueDetail(ctx context.Context, projectID, issueID int64) (IssueDetail, error) {
	var d IssueDetail
	err := s.pool.QueryRow(ctx, `
		SELECT id, title, culprit, status, platform, last_release, event_count, user_count, first_seen, last_seen, status_changed_at, fingerprint,
		       coalesce((SELECT a.kind = 'regressed' FROM issue_activity a WHERE a.issue_id = i.id ORDER BY a.created_at DESC, a.id DESC LIMIT 1), false) AND i.status = 'unresolved'
		FROM issues i WHERE id = $1 AND project_id = $2`, issueID, projectID).
		Scan(&d.ID, &d.Title, &d.Culprit, &d.Status, &d.Platform, &d.LastRelease, &d.EventCount, &d.UserCount, &d.FirstSeen, &d.LastSeen, &d.StatusAt, &d.Fingerprint, &d.Regressed)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Trend = make([]int, 24)
	list := []IssueRow{d.IssueRow}
	if err := s.fillTrends(ctx, list); err != nil {
		return d, err
	}
	d.Trend = list[0].Trend

	d.Environments, err = s.distinct(ctx, `SELECT DISTINCT environment FROM events WHERE issue_id = $1 AND environment <> '' ORDER BY 1 LIMIT 20`, issueID)
	if err != nil {
		return d, err
	}
	d.Releases, err = s.distinct(ctx, `SELECT release FROM events WHERE issue_id = $1 AND release <> '' GROUP BY release ORDER BY max(timestamp) DESC LIMIT 10`, issueID)
	if err != nil {
		return d, err
	}
	if d.Commits, err = s.releaseCommits(ctx, projectID, d.Releases); err != nil {
		return d, err
	}
	if d.Links, err = s.issueLinks(ctx, issueID); err != nil {
		return d, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.kind, coalesce(u.name, u.email, ''), a.release, a.created_at
		FROM issue_activity a LEFT JOIN users u ON u.id = a.user_id
		WHERE a.issue_id = $1 ORDER BY a.created_at DESC, a.id DESC LIMIT 50`, issueID)
	if err != nil {
		return d, err
	}
	d.Activity, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Activity])
	return d, err
}

func (s *Store) distinct(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}

// IssueEvents returns stored events for an issue, newest first, starting
// before the given time (zero for the latest).
func (s *Store) IssueEvents(ctx context.Context, projectID, issueID int64, before time.Time, limit int) ([]json.RawMessage, error) {
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT payload FROM events
		WHERE project_id = $1 AND issue_id = $2 AND timestamp < $3
		ORDER BY timestamp DESC, event_id DESC LIMIT $4`, projectID, issueID, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
}

// SetIssueStatus changes status for issues in a project and records who did
// it. It returns how many issues actually changed.
func (s *Store) SetIssueStatus(ctx context.Context, projectID int64, issueIDs []int64, status string, userID int64) (int64, error) {
	kind := map[string]string{"resolved": "resolved", "ignored": "ignored", "unresolved": "reopened"}[status]
	if kind == "" {
		return 0, fmt.Errorf("unknown status %q", status)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		UPDATE issues SET status = $3, status_changed_at = now()
		WHERE project_id = $1 AND id = ANY($2) AND status <> $3
		RETURNING id`, projectID, issueIDs, status)
	if err != nil {
		return 0, err
	}
	changed, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	for _, id := range changed {
		// NULLIF: a change made by no one in particular (a script, userID 0)
		// is stored as NULL, which the foreign key allows; 0 isn't a user.
		if _, err := tx.Exec(ctx, `INSERT INTO issue_activity (issue_id, kind, user_id) VALUES ($1, $2, NULLIF($3::bigint, 0))`, id, kind, userID); err != nil {
			return 0, err
		}
	}
	return int64(len(changed)), tx.Commit(ctx)
}

func (s *Store) Environments(ctx context.Context, projectID int64) ([]string, error) {
	return s.distinct(ctx, `SELECT DISTINCT environment FROM events WHERE project_id = $1 AND environment <> '' ORDER BY 1 LIMIT 50`, projectID)
}

type AlertSettings struct {
	SlackWebhookURL string    `json:"slack_webhook_url"`
	EmailTo         []string  `json:"email_to"`
	OnNewIssue      bool      `json:"on_new_issue"`
	OnRegression    bool      `json:"on_regression"`
	WebhookURL      string    `json:"webhook_url"`
	WebhookSecret   string    `json:"webhook_secret"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// AlertSettings returns a project's alert settings, or the defaults (alerts
// on, no channels) if they were never saved.
func (s *Store) AlertSettings(ctx context.Context, projectID int64) (AlertSettings, error) {
	a := AlertSettings{OnNewIssue: true, OnRegression: true, EmailTo: []string{}}
	err := s.pool.QueryRow(ctx, `
		SELECT slack_webhook_url, email_to, on_new_issue, on_regression, webhook_url, webhook_secret, updated_at
		FROM alert_settings WHERE project_id = $1`, projectID).
		Scan(&a.SlackWebhookURL, &a.EmailTo, &a.OnNewIssue, &a.OnRegression, &a.WebhookURL, &a.WebhookSecret, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, nil
	}
	return a, err
}

func (s *Store) SaveAlertSettings(ctx context.Context, projectID int64, a AlertSettings) error {
	if a.EmailTo == nil {
		a.EmailTo = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO alert_settings (project_id, slack_webhook_url, email_to, on_new_issue, on_regression, webhook_url, webhook_secret, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (project_id) DO UPDATE SET
			slack_webhook_url = EXCLUDED.slack_webhook_url, email_to = EXCLUDED.email_to,
			on_new_issue = EXCLUDED.on_new_issue, on_regression = EXCLUDED.on_regression,
			webhook_url = EXCLUDED.webhook_url, webhook_secret = EXCLUDED.webhook_secret, updated_at = now()`,
		projectID, a.SlackWebhookURL, a.EmailTo, a.OnNewIssue, a.OnRegression, a.WebhookURL, a.WebhookSecret)
	return err
}

type Delivery struct {
	ID        int64     `json:"id"`
	IssueID   *int64    `json:"issue_id"`
	Kind      string    `json:"kind"`
	Channel   string    `json:"channel"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) RecordDelivery(ctx context.Context, projectID int64, issueID *int64, kind, channel string, deliveryErr error) error {
	msg := ""
	if deliveryErr != nil {
		msg = deliveryErr.Error()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO alert_deliveries (project_id, issue_id, kind, channel, ok, error) VALUES ($1, $2, $3, $4, $5, $6)`,
		projectID, issueID, kind, channel, deliveryErr == nil, msg)
	return err
}

func (s *Store) RecentDeliveries(ctx context.Context, projectID int64, limit int) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, issue_id, kind, channel, ok, error, created_at FROM alert_deliveries
		WHERE project_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Delivery])
}
