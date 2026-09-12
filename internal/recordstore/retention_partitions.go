package recordstore

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// partitionWindowMonths is the count of monthly partitions EnsurePartitions
// keeps ready per table: the previous month, the current month, and the
// next two months. Migration 00001 creates the same four for a fresh
// install.
const partitionWindowMonths = 4

// EnsurePartitions creates the "runs" and "log_chunks" partitions for the
// previous month, the current month, and the next two months, computed
// from now. It is idempotent: CREATE TABLE IF NOT EXISTS skips a month a
// previous round, or migration 00001, already created. A caller's run_id
// can be up to 24 hours old, so the previous month's partition must exist
// on the first day of a new month.
//
// "activity" gets the same four-month window plus every month further
// back to activityRetention, because the first pass imports history the
// retained JetStream buffer already holds, predating the current month.
func EnsurePartitions(ctx context.Context, db maintDB, now time.Time, activityRetention time.Duration) error {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	for i := 0; i < partitionWindowMonths; i++ {
		from := monthStart.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		suffix := from.Format("200601")
		if err := createPartition(ctx, db, "runs", suffix, from, to); err != nil {
			return err
		}
		if err := createPartition(ctx, db, "log_chunks", suffix, from, to); err != nil {
			return err
		}
		if err := createPartition(ctx, db, "activity", suffix, from, to); err != nil {
			return err
		}
	}

	earliest := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-activityRetention)
	earliestMonth := time.Date(earliest.Year(), earliest.Month(), 1, 0, 0, 0, 0, time.UTC)
	for from := earliestMonth; from.Before(monthStart); from = from.AddDate(0, 1, 0) {
		to := from.AddDate(0, 1, 0)
		suffix := from.Format("200601")
		if err := createPartition(ctx, db, "activity", suffix, from, to); err != nil {
			return err
		}
	}
	return nil
}

// createPartition issues one CREATE TABLE IF NOT EXISTS ... PARTITION OF
// statement for table's month named by suffix, covering [from, to). The
// bounds are formatted as literals rather than bind parameters: Postgres
// does not accept bind parameters inside a partition bound expression.
func createPartition(ctx context.Context, db maintDB, table, suffix string, from, to time.Time) error {
	partitionName := fmt.Sprintf("%s_p%s", table, suffix)
	stmt := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS varroa.%s PARTITION OF varroa.%s FOR VALUES FROM ('%s') TO ('%s')",
		partitionName, table, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339),
	)
	if _, err := db.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create partition %s: %w", partitionName, err)
	}
	return nil
}

// partitionInfo is one RANGE partition of a table in schema "varroa",
// with its bounds parsed out of pg_get_expr's text form.
type partitionInfo struct {
	name  string
	lower time.Time
	upper time.Time
}

// partitionBoundPattern matches the text pg_get_expr returns for a RANGE
// partition bound, for example "FOR VALUES FROM ('2026-08-01
// 00:00:00+00') TO ('2026-09-01 00:00:00+00')".
var partitionBoundPattern = regexp.MustCompile(`FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)`)

// pgTimestampLayout matches the text form Postgres prints for a
// timestamptz value inside pg_get_expr's output.
const pgTimestampLayout = "2006-01-02 15:04:05-07"

// listPartitions lists every RANGE partition of varroa.<table>, with its
// parsed lower and upper bound. A partition whose bound text does not
// match the expected two-literal form is skipped, because this package
// creates no partition with any other bound shape.
func listPartitions(ctx context.Context, db maintDB, table string) ([]partitionInfo, error) {
	rows, err := db.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = p.relnamespace
		WHERE n.nspname = 'varroa' AND p.relname = $1
	`, table)
	if err != nil {
		return nil, fmt.Errorf("list partitions of %s: %w", table, err)
	}
	defer rows.Close()

	var partitions []partitionInfo
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, fmt.Errorf("scan partition row: %w", err)
		}
		m := partitionBoundPattern.FindStringSubmatch(bound)
		if m == nil {
			continue
		}
		lower, err := time.Parse(pgTimestampLayout, m[1])
		if err != nil {
			continue
		}
		upper, err := time.Parse(pgTimestampLayout, m[2])
		if err != nil {
			continue
		}
		partitions = append(partitions, partitionInfo{name: name, lower: lower, upper: upper})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list partitions of %s: %w", table, err)
	}
	return partitions, nil
}

// dropExpiredPartitions drops every partition of varroa.<table> whose
// whole range is at or before cutoff, that is, whose upper bound does
// not exceed cutoff. It returns the names it dropped.
func dropExpiredPartitions(ctx context.Context, db maintDB, table string, cutoff time.Time) ([]string, error) {
	partitions, err := listPartitions(ctx, db, table)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, p := range partitions {
		if p.upper.After(cutoff) {
			continue
		}
		if _, err := db.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS varroa.%s", p.name)); err != nil {
			return dropped, fmt.Errorf("drop partition %s: %w", p.name, err)
		}
		dropped = append(dropped, p.name)
	}
	return dropped, nil
}

// oldestRunsPartitionLowerBound returns the lower bound of the oldest
// surviving "runs" partition. The intent sweep (step 7) deletes no
// run_intents row younger than this value, so an intent always outlives
// its run even when monthly rounding keeps the run past retention.runs.
func oldestRunsPartitionLowerBound(ctx context.Context, db maintDB) (time.Time, error) {
	partitions, err := listPartitions(ctx, db, "runs")
	if err != nil {
		return time.Time{}, err
	}
	if len(partitions) == 0 {
		return time.Time{}, fmt.Errorf("no runs partitions exist")
	}
	oldest := partitions[0].lower
	for _, p := range partitions[1:] {
		if p.lower.Before(oldest) {
			oldest = p.lower
		}
	}
	return oldest, nil
}
