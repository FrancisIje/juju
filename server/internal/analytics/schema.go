package analytics

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// schema creates the tables if they don't exist. ClickHouse has no
// migrations tool here, so every change must be safe to run again (IF NOT
// EXISTS, ADD COLUMN IF NOT EXISTS); Migrate runs on every start.
var schema = []string{
	`CREATE DATABASE IF NOT EXISTS {db}`,
	// One row per analytics event.
	//  - ReplacingMergeTree drops rows with the same sorting key when parts
	//    merge, so an event an SDK sent twice (a retry) is eventually stored
	//    once. Until then, counts use uniqExact(event_id), so they're right
	//    either way.
	//  - ORDER BY decides what's fast: "this project's events with this name
	//    over this time" reads one contiguous range.
	//  - Properties are a Map of text: any key, and breakdowns treat every
	//    value the same way.
	`CREATE TABLE IF NOT EXISTS {db}.events (
		project_id  UInt64,
		event_id    UUID,
		name        LowCardinality(String),
		distinct_id String,
		timestamp   DateTime64(3, 'UTC'),
		received_at DateTime64(3, 'UTC'),
		platform    LowCardinality(String),
		lib         LowCardinality(String),
		release     LowCardinality(String),
		environment LowCardinality(String),
		os          LowCardinality(String),
		browser     LowCardinality(String),
		properties  Map(String, String)
	) ENGINE = ReplacingMergeTree
	PARTITION BY toYYYYMM(timestamp)
	ORDER BY (project_id, name, toDate(timestamp), event_id)`,
	// identify(): this distinct_id (an anonymous device id) is this person.
	// The first link wins (queries take argMin by created_at), so a shared
	// device logging into a second account can't move the first account's
	// history. Plain MergeTree, not Replacing: that would keep the newest
	// row after a merge, quietly flipping to "last link wins".
	`CREATE TABLE IF NOT EXISTS {db}.person_aliases (
		project_id  UInt64,
		distinct_id String,
		person_id   String,
		created_at  DateTime64(3, 'UTC')
	) ENGINE = MergeTree
	ORDER BY (project_id, distinct_id)`,
}

// Migrate creates the database and tables.
func (c *Client) Migrate(ctx context.Context) error {
	if !validName.MatchString(c.db()) {
		// The one value written into SQL text instead of passed as a
		// parameter (ClickHouse can't parameterize a database name).
		return fmt.Errorf("clickhouse database name %q: use letters, digits and _", c.db())
	}
	for _, s := range schema {
		if _, err := c.doIn(ctx, "default", strings.ReplaceAll(s, "{db}", c.db()), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
