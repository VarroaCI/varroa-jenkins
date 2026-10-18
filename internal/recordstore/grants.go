package recordstore

import (
	"context"
	"database/sql"
	"fmt"
)

// ensureGrantsSQL grants the read-only client role and the gateway role
// their privileges on schema "varroa", skipping each grant when its role
// does not exist. CloudNativePG creates a role from spec.managed.roles on
// its own schedule, which can run after the first migration pass finishes.
//
// varroa_gateway gets no default INSERT privilege: a default INSERT would
// reach every future table the recorder creates, including the record
// tables, and the recorder must stay their only writer. The explicit
// INSERT on varroa.log_chunks names the partitioned parent only;
// PostgreSQL routes an insert through the parent and checks the parent's
// privileges, so a partition created after this grant needs no grant of
// its own. The client role also writes the four agent tables with INSERT
// and UPDATE only. It holds no DELETE on any table; the recorder's
// maintenance round is the only deleter of agent rows. The UPDATE grant covers digest, bytes, and writer alone: the
// log writer replaces a log part that a lost writer left above a hole,
// and needs no other column. varroa_gateway gets no DELETE.
const ensureGrantsSQL = `
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'varroa_client') THEN
    GRANT USAGE ON SCHEMA varroa TO varroa_client;
    GRANT SELECT ON ALL TABLES IN SCHEMA varroa TO varroa_client;
    GRANT INSERT ON varroa.log_chunks TO varroa_client;
    GRANT INSERT, UPDATE ON varroa.agent_sessions, varroa.agent_session_events,
      varroa.agent_approvals, varroa.agent_settings TO varroa_client;
    ALTER DEFAULT PRIVILEGES IN SCHEMA varroa GRANT SELECT ON TABLES TO varroa_client;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'varroa_gateway') THEN
    GRANT USAGE ON SCHEMA varroa TO varroa_gateway;
    GRANT SELECT ON ALL TABLES IN SCHEMA varroa TO varroa_gateway;
    GRANT INSERT ON varroa.log_chunks TO varroa_gateway;
    GRANT UPDATE (digest, bytes, writer) ON varroa.log_chunks TO varroa_gateway;
    ALTER DEFAULT PRIVILEGES IN SCHEMA varroa GRANT SELECT ON TABLES TO varroa_gateway;
  END IF;
END $$;
`

// EnsureGrants grants roles "varroa_client" and "varroa_gateway" their
// privileges on schema "varroa" when a role exists, and does nothing for a
// role that does not. It creates no role. It is idempotent, so the
// migration runner calls it after every goose.Up, and the retention ticker
// repeats it every round until a role appears.
func EnsureGrants(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ensureGrantsSQL); err != nil {
		return fmt.Errorf("ensure grants: %w", err)
	}
	return nil
}
