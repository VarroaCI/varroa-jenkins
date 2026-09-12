package recordstore

import "embed"

// migrationsDir is the directory name goose reads migration files from,
// relative to the root of migrationsFS.
const migrationsDir = "migrations"

// migrationsFS embeds every goose migration file so the recorder ships them
// inside its own binary. It needs no separate deploy artifact.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS
