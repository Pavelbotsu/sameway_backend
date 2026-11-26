# sameway_backend (Go): build and test

Project rules are in `../CLAUDE.md`. This file only has commands.

- Docker Desktop must be running (it doesn't auto-start). If testcontainers or `dev.sh` fail with "dockerDesktopLinuxEngine … cannot find the file", start `C:\Program Files\Docker\Docker\Docker Desktop.exe` and wait for `docker info`.
- Secrets: only from env vars / `.env` (ignored, as is `serviceAccountKey.json`). Run `git config core.hooksPath .githooks` once per clone: the pre-commit hook runs gitleaks (`go install github.com/zricethezav/gitleaks/v8@v8.21.2`). CI scans the full history (`.github/workflows/gitleaks.yml`); checked non-secrets go in `.gitleaksignore`.
- Dev server: `./scripts/dev.sh` → PostGIS container `rideshare-db` on 127.0.0.1:55432, API on http://localhost:4040 (emulator: `10.0.2.2:4040`).
- Check: `go build ./...` and `go vet ./...`
- Test: `go test ./... -count=1 -p 1`. Run packages serially: parallel testcontainers flake on Windows ("rootless Docker is not supported"). A package panicking with `reaper … unexpected container status "removing"` is a flake; re-run that package alone. No `-race` (no CGO here).
- Single test: `go test ./handlers/ -run TestName -count=1 -v`
- New tables: add them to the TRUNCATE list in `testutil.ResetDB` and to the retention rules (`handlers/retention.go`).
- Routes: only in `routes/routes.go`. Tests use `testutil.NewTestRouter()` (no rate limits) or `NewTestRouterWithRateLimits()`.
- Schema: until the golang-migrate PR lands, the schema is in `db/postgres.go`. `alterSQL` is **frozen**; new schema changes wait for (or go into) migrations. After that PR: test every migration up, down and up again.
- Admin CLI: `go run ./cmd/admin <command>` locally; in prod `docker compose -f docker-compose.prod.yml exec app /admin <command>`.
