# CLAUDE.md — horus

Small internal DNS server in Go + a Kubernetes operator. Read `docs/DESIGN.md`
(decisions, architecture, rules) and `docs/API.md` (REST contract) before
changing behavior. The "Decisions" table in DESIGN.md is agreed with the owner;
do not change those without asking.

## Current state

Everything compiles against the real dependencies (Go 1.26, miekg/dns,
modernc.org/sqlite, controller-runtime). `make vet test operator-test` and
`make e2e` (kind) pass. Nothing is deployed yet; there is no CI.

Local smoke test without root (macOS: port 5353 is taken by mDNSResponder,
and unix socket paths must stay under 104 bytes):
`HORUS_DNS_LISTEN=127.0.0.1:15353 HORUS_API_SOCKET=$PWD/api.sock HORUS_DB=$PWD/dev.db HORUS_ADMIN_TOKEN_FILE=<file with 40 chars> horus serve`,
then `HORUS_ADDR=unix://$PWD/api.sock HORUS_TOKEN_FILE=<same file> horus zone add ...`
and `dig @127.0.0.1 -p 15353 ...`.

## Commands

```sh
make tidy            # go mod tidy (root + operator)
make vet test        # root module
make operator-test   # operator module
make build operator  # bin/horus, bin/horus-operator
make e2e             # kind e2e (docker, kind, kubectl, dig); KEEP=1 keeps it up
```

## Layout and boundaries

- `pkg/*` is shared with the operator module and must stay **stdlib-only**.
- `internal/store` uses only `database/sql`; the driver is imported in
  `cmd/horus/sqlite.go`. Keep it that way so store logic stays testable.
- `internal/zone` must not import a DNS library; it speaks `api.Record` and
  qtype numbers. Resolution rules belong there and are tested there.
- The DNS hot path (`dnsserver.Handler`) must never touch SQLite or take a lock.
- All writes go through `httpapi.Server.write`/`writeIf` (mutex → store tx →
  snapshot reload). Never write to the DB from anywhere else.
- Ownership/first-wins logic lives in `internal/store/plan.go` (pure). Change it
  there, with tests.
- Operator is a separate module (`operator/go.mod`, `replace ../`) so the
  server binary never links client-go.

## Conventions

- Names are always normalized via `api.NormalizeName` (lowercase, trailing dot)
  before comparison or storage.
- Validation errors wrap `api.ErrInvalid` → HTTP 422. Store errors:
  `store.ErrNotFound` → 404, `store.ErrExists` → 409, `*store.ConflictError` → 409.
- Logging: `log/slog` JSON to stderr (journald). No fmt.Println in the server.
- No new dependencies without a reason; prefer stdlib (that's why metrics and
  sd_notify are hand-rolled).
- Tests next to code, table-driven where it helps. `go test -race` must pass.
- Code, comments and docs in English.

## Known gaps / next steps

- CI (deliberately none yet). When added: `make vet test operator-test`;
  `make e2e` needs docker + kind on the runner.
- `GetEventRecorderFor` is deprecated in recent controller-runtime; it still
  compiles, keep it until it does not.
- `horus backup` twice within one second fails (same file name); harmless
  for the daily timer.
