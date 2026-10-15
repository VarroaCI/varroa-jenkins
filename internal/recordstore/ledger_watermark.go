package recordstore

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
)

// bigintMax is the maximum value a Postgres bigint column holds. No
// higher build number remains to reserve once next_number reaches it.
const bigintMax = math.MaxInt64

// raiseWatermark ensures a job_numbers row exists and raises its
// next_number to minimum when that value is higher. It never lowers an
// existing value.
func raiseWatermark(ctx context.Context, db DBTX, cluster, tenant, jobPath string, minimum int64) error {
	if _, err := db.Exec(ctx, `
		INSERT INTO varroa.job_numbers (cluster, tenant, job_path, next_number)
		VALUES ($1, $2, $3, GREATEST(1, $4))
		ON CONFLICT (cluster, tenant, job_path)
		DO UPDATE SET next_number = GREATEST(varroa.job_numbers.next_number, $4), updated_at = now()
	`, cluster, tenant, jobPath, minimum); err != nil {
		return fmt.Errorf("raise job_numbers watermark: %w", err)
	}
	return nil
}

// reserveBuildNumber raises the watermark to jenkinsNext when that value
// is higher, then reserves the current next_number and advances it by
// one. It is the only path that hands out a build number.
func reserveBuildNumber(ctx context.Context, db DBTX, cluster, tenant, jobPath string, jenkinsNext int64) (int64, error) {
	if err := raiseWatermark(ctx, db, cluster, tenant, jobPath, jenkinsNext); err != nil {
		return 0, err
	}

	var reserved int64
	if err := db.QueryRow(ctx, `
		UPDATE varroa.job_numbers
		SET next_number = next_number + 1, updated_at = now()
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
		RETURNING next_number - 1
	`, cluster, tenant, jobPath).Scan(&reserved); err != nil {
		return 0, fmt.Errorf("reserve build number: %w", err)
	}
	return reserved, nil
}

// RaiseWatermarkForStartedBuild raises job_numbers.next_number to
// buildNumber+1 when that value is higher. A run.started handler calls it
// in the same transaction that writes the run, so the watermark never
// falls behind a number Jenkins already assigned.
func RaiseWatermarkForStartedBuild(ctx context.Context, db DBTX, cluster, tenant, jobPath string, buildNumber int64) error {
	return raiseWatermark(ctx, db, cluster, tenant, jobPath, buildNumber+1)
}

// NextBuildNumber reads job_numbers.next_number for a job. It returns 1
// for a job with no row. It returns an error when the stored value
// equals the bigint maximum, because no higher number remains to
// reserve.
func NextBuildNumber(ctx context.Context, db DBTX, cluster, tenant, jobPath string) (int64, error) {
	var next int64
	err := db.QueryRow(ctx, `
		SELECT next_number FROM varroa.job_numbers
		WHERE cluster = $1 AND tenant = $2 AND job_path = $3
	`, cluster, tenant, jobPath).Scan(&next)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 1, nil
	case err != nil:
		return 0, fmt.Errorf("read next build number: %w", err)
	}
	if next == bigintMax {
		return 0, fmt.Errorf("job_numbers.next_number is at the bigint maximum for %s/%s/%s", cluster, tenant, jobPath)
	}
	return next, nil
}
