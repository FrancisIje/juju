package analytics

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// personEvents is every event of one project with the person it belongs
// to. identify() links an anonymous id to a user id; an event's person is
// that user if its distinct_id was linked (the first link wins, argMin),
// else the distinct_id itself. So one person's events before and after
// logging in count as one person.
const personEvents = `aliases AS (
		SELECT distinct_id, argMin(person_id, created_at) AS pid
		FROM person_aliases WHERE project_id = {p:UInt64} GROUP BY distinct_id
	),
	pe AS (
		SELECT e.event_id AS event_id, e.name AS name, e.timestamp AS timestamp,
		       e.lib AS lib, e.os AS os, e.browser AS browser, e.release AS release,
		       e.environment AS environment, e.properties AS properties,
		       if(a.pid != '', a.pid, e.distinct_id) AS person
		FROM events AS e LEFT JOIN aliases AS a ON a.distinct_id = e.distinct_id
		WHERE e.project_id = {p:UInt64}
		  AND e.timestamp >= fromUnixTimestamp64Milli({from:Int64}, 'UTC')
		  AND e.timestamp < fromUnixTimestamp64Milli({to:Int64}, 'UTC')
	)`

// Range is the time window a question covers, [From, To).
type Range struct{ From, To time.Time }

func (r Range) params(projectID int64) map[string]string {
	return map[string]string{
		"p":    strconv.FormatInt(projectID, 10),
		"from": strconv.FormatInt(r.From.UnixMilli(), 10),
		"to":   strconv.FormatInt(r.To.UnixMilli(), 10),
	}
}

// ErrBadQuery is a question the API can't answer as asked; the message
// says why, in words for the person asking.
type ErrBadQuery struct{ Msg string }

func (e ErrBadQuery) Error() string { return e.Msg }

// Interval is how wide each point of a trend is.
type Interval string

const (
	Hour Interval = "hour"
	Day  Interval = "day"
	Week Interval = "week"
)

// bucketSQL turns a timestamp into its bucket's start, as Unix seconds. From
// a fixed list: an interval is never pasted into SQL from input.
var bucketSQL = map[Interval]string{
	Hour: "toUnixTimestamp(toStartOfHour(timestamp))",
	Day:  "toUnixTimestamp(toDateTime(toDate(timestamp)))",
	Week: "toUnixTimestamp(toDateTime(toMonday(timestamp)))", // weeks start on Monday
}

// Buckets lists every bucket start in the range, so days with no events
// show as 0 instead of being missing from the chart.
func Buckets(r Range, iv Interval) []time.Time {
	t := r.From.UTC()
	switch iv {
	case Hour:
		t = t.Truncate(time.Hour)
	case Day:
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case Week:
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		t = t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	}
	var out []time.Time
	for ; t.Before(r.To); t = step(t, iv) {
		out = append(out, t)
	}
	return out
}

func step(t time.Time, iv Interval) time.Time {
	switch iv {
	case Hour:
		return t.Add(time.Hour)
	case Week:
		return t.AddDate(0, 0, 7)
	}
	return t.AddDate(0, 0, 1)
}

// Breakdown columns that can be grouped by, besides event properties.
var breakdownSQL = map[string]string{
	"":            "''",
	"lib":         "lib",
	"os":          "os",
	"browser":     "browser",
	"release":     "release",
	"environment": "environment",
}

const propertyPrefix = "property:"

func breakdownExpr(b string, params map[string]string) (string, error) {
	if key, ok := strings.CutPrefix(b, propertyPrefix); ok {
		if key == "" {
			return "", ErrBadQuery{"choose a property to break down by"}
		}
		params["bkey"] = key
		return "properties[{bkey:String}]", nil
	}
	expr, ok := breakdownSQL[b]
	if !ok {
		return "", ErrBadQuery{fmt.Sprintf("can't break down by %q", b)}
	}
	return expr, nil
}

type TrendQuery struct {
	Event     string
	Range     Range
	Interval  Interval
	People    bool   // count unique people instead of events
	Breakdown string // "", lib, os, browser, release, environment, or property:<key>
}

type Series struct {
	Label  string  `json:"label"`
	Values []int64 `json:"values"`
	Total  int64   `json:"total"` // this line over the whole range, each person once
}

type TrendResult struct {
	Buckets []time.Time `json:"buckets"`
	Series  []Series    `json:"series"`
	// Total is the whole answer for the range: every event, or every
	// different person, including breakdown values not shown. It can't be
	// worked out from Series: someone active on two days is one person, not
	// two, and someone on both iOS and Android is in both of those lines.
	Total int64 `json:"total"`
	// More breakdown values existed than are shown (the top MaxSeries are).
	Hidden int `json:"hidden"`
}

const MaxSeries = 10

// Trend counts an event per bucket, optionally one line per breakdown value.
func (c *Client) Trend(ctx context.Context, projectID int64, q TrendQuery) (TrendResult, error) {
	bucket, ok := bucketSQL[q.Interval]
	if !ok {
		return TrendResult{}, ErrBadQuery{"interval must be hour, day or week"}
	}
	if q.Event == "" {
		return TrendResult{}, ErrBadQuery{"choose an event"}
	}
	params := q.Range.params(projectID)
	params["event"] = q.Event
	label, err := breakdownExpr(q.Breakdown, params)
	if err != nil {
		return TrendResult{}, err
	}
	value := "uniqExact(event_id)" // a retried event is counted once, merged or not
	if q.People {
		value = "uniqExact(person)"
	}
	type row struct {
		Bucket int64  `json:"bucket"`
		Label  string `json:"label"`
		Value  int64  `json:"value"`
	}
	rows, err := Query[row](ctx, c, `WITH `+personEvents+`
		SELECT `+bucket+` AS bucket, toString(`+label+`) AS label, `+value+` AS value
		FROM pe WHERE name = {event:String}
		GROUP BY bucket, label`, params)
	if err != nil {
		return TrendResult{}, err
	}

	buckets := Buckets(q.Range, q.Interval)
	index := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		index[b.Unix()] = i
	}
	byLabel := map[string]*Series{}
	for _, r := range rows {
		s := byLabel[r.Label]
		if s == nil {
			s = &Series{Label: r.Label, Values: make([]int64, len(buckets))}
			byLabel[r.Label] = s
		}
		if i, ok := index[r.Bucket]; ok {
			s.Values[i] += r.Value
		}
	}
	// Totals are counted again over the whole range, not added up from the
	// buckets: unique counts don't add.
	type totalRow struct {
		Label string `json:"label"`
		All   uint8  `json:"all"`
		Value int64  `json:"value"`
	}
	totals, err := Query[totalRow](ctx, c, `WITH `+personEvents+`
		SELECT toString(`+label+`) AS label, 0 AS all, `+value+` AS value
		FROM pe WHERE name = {event:String} GROUP BY label
		UNION ALL
		SELECT '' AS label, 1 AS all, `+value+` AS value
		FROM pe WHERE name = {event:String}`, params)
	if err != nil {
		return TrendResult{}, err
	}
	var total int64
	for _, r := range totals {
		if r.All == 1 {
			total = r.Value
		} else if s := byLabel[r.Label]; s != nil {
			s.Total = r.Value
		}
	}
	series := make([]Series, 0, len(byLabel))
	for _, s := range byLabel {
		series = append(series, *s)
	}
	// Biggest first; ties by name, so the order is stable between reloads.
	slices.SortFunc(series, func(a, b Series) int {
		if a.Total != b.Total {
			return cmp.Compare(b.Total, a.Total)
		}
		return strings.Compare(a.Label, b.Label)
	})
	res := TrendResult{Buckets: buckets, Series: series, Total: total}
	if len(series) > MaxSeries {
		res.Series, res.Hidden = series[:MaxSeries], len(series)-MaxSeries
	}
	if len(res.Series) == 0 {
		res.Series = []Series{{Label: "", Values: make([]int64, len(buckets))}}
	}
	return res, nil
}

type FunnelQuery struct {
	Steps  []string
	Range  Range
	Window time.Duration // how long a person has from the first step to the last
}

type FunnelStep struct {
	Event  string `json:"event"`
	People int64  `json:"people"`
}

const maxFunnelSteps = 8

// Funnel counts how many people did step 1, then step 2 after it, and so on,
// within Window of their first step. windowFunnel does the ordering per
// person; people who reached step k count for every step up to k. It compares
// milliseconds: whole seconds would count a step taken just before the
// previous one as being after it. Steps in the same millisecond count as in
// order, since nothing says otherwise.
func (c *Client) Funnel(ctx context.Context, projectID int64, q FunnelQuery) ([]FunnelStep, error) {
	if len(q.Steps) < 2 || len(q.Steps) > maxFunnelSteps {
		return nil, ErrBadQuery{fmt.Sprintf("a funnel needs 2 to %d steps", maxFunnelSteps)}
	}
	if q.Window <= 0 {
		return nil, ErrBadQuery{"the conversion window must be longer than 0"}
	}
	params := q.Range.params(projectID)
	params["window"] = strconv.FormatInt(q.Window.Milliseconds(), 10)
	conds := make([]string, len(q.Steps))
	names := make([]string, len(q.Steps))
	for i, s := range q.Steps {
		if s == "" {
			return nil, ErrBadQuery{fmt.Sprintf("step %d has no event", i+1)}
		}
		k := "s" + strconv.Itoa(i)
		params[k] = s
		conds[i] = "name = {" + k + ":String}"
		names[i] = "{" + k + ":String}"
	}
	type row struct {
		Level  int   `json:"level"`
		People int64 `json:"people"`
	}
	rows, err := Query[row](ctx, c, `WITH `+personEvents+`
		SELECT level, count() AS people FROM (
			SELECT person, windowFunnel({window:UInt64})(toUInt64(toUnixTimestamp64Milli(timestamp)), `+strings.Join(conds, ", ")+`) AS level
			FROM pe WHERE name IN (`+strings.Join(names, ", ")+`)
			GROUP BY person
		) GROUP BY level`, params)
	if err != nil {
		return nil, err
	}
	out := make([]FunnelStep, len(q.Steps))
	for i, s := range q.Steps {
		out[i].Event = s
	}
	for _, r := range rows {
		for k := 0; k < r.Level && k < len(out); k++ {
			out[k].People += r.People
		}
	}
	return out, nil
}

type RetentionQuery struct {
	Start, Return string // e.g. "signed_up" then "opened_app"
	Range         Range  // cohorts: people whose first Start event is in here
	Periods       int    // days after the cohort day to check (1..Periods)
}

type Cohort struct {
	Day      time.Time `json:"day"`
	People   int64     `json:"people"`
	Returned []int64   `json:"returned"` // Returned[i]: people who did Return on day i+1
}

// Retention groups people by the day of their first Start event, then
// counts, for each following day, how many of them did Return that day.
func (c *Client) Retention(ctx context.Context, projectID int64, q RetentionQuery) ([]Cohort, error) {
	if q.Start == "" || q.Return == "" {
		return nil, ErrBadQuery{"choose a starting event and a returning event"}
	}
	if q.Periods < 1 || q.Periods > 31 {
		return nil, ErrBadQuery{"periods must be 1 to 31 days"}
	}
	// Returns can happen up to Periods days after the last cohort day.
	wide := Range{From: q.Range.From, To: q.Range.To.AddDate(0, 0, q.Periods)}
	params := wide.params(projectID)
	params["start"], params["ret"] = q.Start, q.Return
	params["cfrom"] = strconv.FormatInt(q.Range.From.UnixMilli(), 10)
	params["cto"] = strconv.FormatInt(q.Range.To.UnixMilli(), 10)
	params["periods"] = strconv.Itoa(q.Periods)
	type row struct {
		Cohort int64 `json:"cohort"`
		Period int   `json:"period"`
		People int64 `json:"people"`
	}
	rows, err := Query[row](ctx, c, `WITH `+personEvents+`,
	starts AS (
		SELECT person, min(toDate(timestamp)) AS cohort FROM pe
		WHERE name = {start:String}
		  AND timestamp >= fromUnixTimestamp64Milli({cfrom:Int64}, 'UTC')
		  AND timestamp < fromUnixTimestamp64Milli({cto:Int64}, 'UTC')
		GROUP BY person
	),
	returns AS (
		SELECT DISTINCT person, toDate(timestamp) AS day FROM pe WHERE name = {ret:String}
	)
	SELECT toUnixTimestamp(toDateTime(cohort)) AS cohort, 0 AS period, count() AS people FROM starts GROUP BY cohort
	UNION ALL
	SELECT toUnixTimestamp(toDateTime(s.cohort)) AS cohort, toInt32(dateDiff('day', s.cohort, r.day)) AS period, uniqExact(s.person) AS people
	FROM starts AS s INNER JOIN returns AS r ON r.person = s.person
	WHERE r.day > s.cohort AND dateDiff('day', s.cohort, r.day) <= {periods:UInt32}
	GROUP BY cohort, period`, params)
	if err != nil {
		return nil, err
	}
	byDay := map[int64]*Cohort{}
	for _, d := range Buckets(q.Range, Day) {
		byDay[d.Unix()] = &Cohort{Day: d, Returned: make([]int64, q.Periods)}
	}
	for _, r := range rows {
		c := byDay[r.Cohort]
		if c == nil {
			continue
		}
		if r.Period == 0 {
			c.People = r.People
		} else if r.Period <= q.Periods {
			c.Returned[r.Period-1] = r.People
		}
	}
	out := make([]Cohort, 0, len(byDay))
	for _, d := range Buckets(q.Range, Day) {
		out = append(out, *byDay[d.Unix()])
	}
	return out, nil
}

type EventName struct {
	Name     string `json:"name"`
	Count    int64  `json:"count"`
	LastSeen int64  `json:"last_seen"` // Unix seconds
}

// EventNames lists the events seen in the range, most frequent first: the
// choices in the dashboard's event pickers.
func (c *Client) EventNames(ctx context.Context, projectID int64, r Range) ([]EventName, error) {
	return Query[EventName](ctx, c, `SELECT name, uniqExact(event_id) AS count, toUnixTimestamp(max(timestamp)) AS last_seen
		FROM events WHERE project_id = {p:UInt64}
		  AND timestamp >= fromUnixTimestamp64Milli({from:Int64}, 'UTC')
		  AND timestamp < fromUnixTimestamp64Milli({to:Int64}, 'UTC')
		GROUP BY name ORDER BY count DESC, name LIMIT 500`, r.params(projectID))
}

// PropertyKeys lists the property names an event has been sent with.
func (c *Client) PropertyKeys(ctx context.Context, projectID int64, eventName string, r Range) ([]string, error) {
	params := r.params(projectID)
	params["event"] = eventName
	type row struct {
		Key string `json:"key"`
	}
	rows, err := Query[row](ctx, c, `SELECT arrayJoin(mapKeys(properties)) AS key, count() AS n
		FROM events WHERE project_id = {p:UInt64} AND name = {event:String}
		  AND timestamp >= fromUnixTimestamp64Milli({from:Int64}, 'UTC')
		  AND timestamp < fromUnixTimestamp64Milli({to:Int64}, 'UTC')
		GROUP BY key ORDER BY n DESC, key LIMIT 200`, params)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.Key
	}
	return keys, nil
}

// IsBadQuery reports whether err is the caller's mistake (answer 400).
func IsBadQuery(err error) bool {
	var b ErrBadQuery
	return errors.As(err, &b)
}
