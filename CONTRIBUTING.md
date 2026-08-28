# Contributing to Varroa

Thanks for your interest in contributing.

## Development setup

Start with `AGENTS.md` — it documents the build/test/lint commands, the
architecture map, and the non-obvious gotchas you'll want to know before
touching `internal/mite/` or the reconcile loop.

Quick reference:

```bash
make build          # compile bin/varroa-jenkins
make test           # all Go tests with race detector
make lint           # golangci-lint
make localdev       # full local stack on kind

cd frontend && npm ci && npm run dev   # frontend dev server
```

## Making a change

1. Fork the repo and create a branch off `main`.
2. Make your change, keeping docs under `docs/` in sync with any behavior
   change in the same PR.
3. Run `make lint` and `make test` locally; for a frontend change, also run
   `npm run coverage` and `npm run build` from `frontend/`.
4. Open a pull request against `main`. CI runs lint, tests, and builds
   automatically.

## Code of conduct

Be respectful and constructive in issues, discussions, and reviews. Focus
feedback on the code, not the contributor.
