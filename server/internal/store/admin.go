package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queries for the super admin view: every organization and project on the
// server. Unlike the rest of the dashboard's queries these aren't scoped to
// one project, so they return only counts and names. Issue titles, event
// payloads, keys and webhook URLs stay out: they're customers' data, and
// watching the health of a server doesn't need them.

// SetSuperAdmin grants or revokes super admin for the account with this email.
func (s *Store) SetSuperAdmin(ctx context.Context, email string, on bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET is_superadmin = $2 WHERE email = $1`, email, on)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SuperAdmins lists the accounts that are super admins.
func (s *Store) SuperAdmins(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, email, name, created_at, is_superadmin FROM users WHERE is_superadmin ORDER BY email`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[User])
}

// What a project's status can be. See projectStatus.
const (
	StatusActive    = "active"
	StatusQuiet     = "quiet"
	StatusNoEvents  = "no_events"
	StatusNearQuota = "near_quota"
	StatusOverQuota = "over_quota"
	// StatusNoProjects only applies to organizations.
	StatusNoProjects = "no_projects"
)

// QuietAfter is how long a project can send nothing before it counts as
// quiet: long enough that a weekend doesn't set it off, short enough to
// notice an app that stopped reporting.
const QuietAfter = 7 * 24 * time.Hour

type AdminProject struct {
	ID              int64      `json:"id"`
	OrgID           int64      `json:"organization_id"`
	Org             string     `json:"organization"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"created_at"`
	Status          string     `json:"status"`
	Unresolved      int64      `json:"unresolved"`
	EventsThisMonth int64      `json:"events_this_month"`
	EventsLastMonth int64      `json:"events_last_month"`
	EventsAllTime   int64      `json:"-"`
	MonthlyQuota    *int64     `json:"monthly_quota"`
	RateLimit       *int       `json:"rate_limit"`
	Errors24h       int64      `json:"errors_24h"`
	Sessions24h     int64      `json:"sessions_24h"`
	Crashed24h      int64      `json:"crashed_24h"`
	LastEventAt     *time.Time `json:"last_event_at"`
}

type AdminOrg struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"created_at"`
	OwnerEmail      string     `json:"owner_email"`
	Members         int64      `json:"members"`
	Projects        int64      `json:"projects"`
	Status          string     `json:"status"`
	Unresolved      int64      `json:"unresolved"`
	EventsThisMonth int64      `json:"events_this_month"`
	Errors24h       int64      `json:"errors_24h"`
	LastEventAt     *time.Time `json:"last_event_at"`
}

type AdminTotals struct {
	Organizations   int64 `json:"organizations"`
	NewOrgs7d       int64 `json:"new_organizations_7d"`
	Projects        int64 `json:"projects"`
	NewProjects7d   int64 `json:"new_projects_7d"`
	Users           int64 `json:"users"`
	NewUsers7d      int64 `json:"new_users_7d"`
	EventsThisMonth int64 `json:"events_this_month"`
	EventsLastMonth int64 `json:"events_last_month"`
	Errors24h       int64 `json:"errors_24h"`
	Unresolved      int64 `json:"unresolved"`
	// How many projects have each status, keyed by the Status constants.
	ByStatus map[string]int64 `json:"by_status"`
}

type AdminOverview struct {
	GeneratedAt   time.Time      `json:"generated_at"`
	Totals        AdminTotals    `json:"totals"`
	Organizations []AdminOrg     `json:"organizations"`
	Projects      []AdminProject `json:"projects"`
}

// projectStatus sums up a project in one word. Quota comes first because it
// means events are being refused right now, whatever else is going on.
func projectStatus(p AdminProject, now time.Time) string {
	if q := p.MonthlyQuota; q != nil {
		if p.EventsThisMonth >= *q {
			return StatusOverQuota
		}
		if p.EventsThisMonth*5 >= *q*4 { // 80 %, in whole numbers
			return StatusNearQuota
		}
	}
	if p.LastEventAt == nil && p.EventsAllTime == 0 {
		return StatusNoEvents
	}
	if p.LastEventAt == nil || now.Sub(*p.LastEventAt) > QuietAfter {
		return StatusQuiet
	}
	return StatusActive
}

// orgStatus: an organization is active if any of its projects is receiving
// events, and only quiet if it once did and none does now. The quota statuses
// stay on the projects: a project being limited says nothing about the rest.
func orgStatus(projects []AdminProject) string {
	if len(projects) == 0 {
		return StatusNoProjects
	}
	status := StatusNoEvents
	for _, p := range projects {
		switch p.Status {
		case StatusActive, StatusNearQuota, StatusOverQuota:
			return StatusActive
		case StatusQuiet:
			status = StatusQuiet
		}
	}
	return status
}

func (s *Store) adminProjects(ctx context.Context, now time.Time) ([]AdminProject, error) {
	month := MonthStart(now)
	// Each block reads one table once, grouped by project. The two that look
	// at the last 24 hours use the time indexes the retention job added, so
	// their cost follows one day's data rather than the size of the table.
	//
	// issues.last_seen comes from the SDK's clock, so it's capped at now. It
	// covers projects whose last event came before last_accepted_at existed.
	// The cap has to be inside the grouped block, where a row always has a
	// last_seen: LEAST ignores NULLs, so outside it, a project with no issues
	// would get LEAST(NULL, now) = now and look like it just sent an event.
	rows, err := s.pool.Query(ctx, `
		WITH usage AS (
			SELECT project_id,
			       sum(accepted)::bigint AS all_time,
			       coalesce(sum(accepted) FILTER (WHERE month = $1::date), 0)::bigint AS this_month,
			       coalesce(sum(accepted) FILTER (WHERE month = $2::date), 0)::bigint AS last_month,
			       max(last_accepted_at) AS last_accepted
			FROM project_usage GROUP BY project_id
		), issue AS (
			SELECT project_id, count(*) FILTER (WHERE status = 'unresolved') AS unresolved, LEAST(max(last_seen), $4) AS last_seen
			FROM issues GROUP BY project_id
		), err AS (
			SELECT project_id, count(*) AS n FROM events WHERE received_at > $3 GROUP BY project_id
		), ses AS (
			SELECT project_id, count(*) AS n, count(*) FILTER (WHERE status = 'crashed') AS crashed
			FROM app_sessions WHERE started_at > $3 GROUP BY project_id
		)
		SELECT p.id, p.organization_id, o.name, p.name, p.created_at,
		       coalesce(i.unresolved, 0), coalesce(u.this_month, 0), coalesce(u.last_month, 0), coalesce(u.all_time, 0),
		       p.monthly_quota, p.rate_limit,
		       coalesce(e.n, 0), coalesce(s.n, 0), coalesce(s.crashed, 0),
		       GREATEST(u.last_accepted, i.last_seen)
		FROM projects p
		JOIN organizations o ON o.id = p.organization_id
		LEFT JOIN usage u ON u.project_id = p.id
		LEFT JOIN issue i ON i.project_id = p.id
		LEFT JOIN err e ON e.project_id = p.id
		LEFT JOIN ses s ON s.project_id = p.id
		ORDER BY o.name, p.name`,
		month, month.AddDate(0, -1, 0), now.Add(-24*time.Hour), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminProject{}
	for rows.Next() {
		var p AdminProject
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Org, &p.Name, &p.CreatedAt,
			&p.Unresolved, &p.EventsThisMonth, &p.EventsLastMonth, &p.EventsAllTime,
			&p.MonthlyQuota, &p.RateLimit,
			&p.Errors24h, &p.Sessions24h, &p.Crashed24h,
			&p.LastEventAt); err != nil {
			return nil, err
		}
		p.Status = projectStatus(p, now)
		out = append(out, p)
	}
	return out, rows.Err()
}

// summarize adds up the projects into their organizations and the totals.
// orgs must hold each organization's own fields already.
func summarize(orgs []AdminOrg, projects []AdminProject) AdminTotals {
	t := AdminTotals{ByStatus: map[string]int64{}}
	byOrg := map[int64][]AdminProject{}
	for _, p := range projects {
		byOrg[p.OrgID] = append(byOrg[p.OrgID], p)
		t.Projects++
		t.ByStatus[p.Status]++
		t.EventsThisMonth += p.EventsThisMonth
		t.EventsLastMonth += p.EventsLastMonth
		t.Errors24h += p.Errors24h
		t.Unresolved += p.Unresolved
	}
	for i := range orgs {
		o := &orgs[i]
		o.Status = orgStatus(byOrg[o.ID])
		for _, p := range byOrg[o.ID] {
			o.Projects++
			o.Unresolved += p.Unresolved
			o.EventsThisMonth += p.EventsThisMonth
			o.Errors24h += p.Errors24h
			if p.LastEventAt != nil && (o.LastEventAt == nil || p.LastEventAt.After(*o.LastEventAt)) {
				o.LastEventAt = p.LastEventAt
			}
		}
	}
	t.Organizations = int64(len(orgs))
	return t
}

// AdminOverview is everything on the super admin page, in one answer. Ingest
// counts events a few seconds after accepting them, so the event numbers can
// be that far behind.
func (s *Store) AdminOverview(ctx context.Context, now time.Time) (AdminOverview, error) {
	projects, err := s.adminProjects(ctx, now)
	if err != nil {
		return AdminOverview{}, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.name, o.created_at,
		       coalesce((SELECT u.email FROM memberships m JOIN users u ON u.id = m.user_id
		                 WHERE m.organization_id = o.id AND m.role = 'owner'
		                 ORDER BY m.created_at LIMIT 1), ''),
		       (SELECT count(*) FROM memberships m WHERE m.organization_id = o.id)
		FROM organizations o ORDER BY o.name`)
	if err != nil {
		return AdminOverview{}, err
	}
	defer rows.Close()
	orgs := []AdminOrg{}
	for rows.Next() {
		var o AdminOrg
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt, &o.OwnerEmail, &o.Members); err != nil {
			return AdminOverview{}, err
		}
		orgs = append(orgs, o)
	}
	if err := rows.Err(); err != nil {
		return AdminOverview{}, err
	}
	t := summarize(orgs, projects)
	week := now.Add(-7 * 24 * time.Hour)
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users),
		       (SELECT count(*) FROM users WHERE created_at > $1),
		       (SELECT count(*) FROM organizations WHERE created_at > $1),
		       (SELECT count(*) FROM projects WHERE created_at > $1)`, week,
	).Scan(&t.Users, &t.NewUsers7d, &t.NewOrgs7d, &t.NewProjects7d)
	if err != nil {
		return AdminOverview{}, err
	}
	return AdminOverview{GeneratedAt: now, Totals: t, Organizations: orgs, Projects: projects}, nil
}
