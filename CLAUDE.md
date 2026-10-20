# CLAUDE.md — horus

Small internal DNS server in Go + a Kubernetes operator. Read `docs/DESIGN.md`
(decisions, architecture, rules) and `docs/API.md` (REST contract) before
changing behavior. The "Decisions" table in DESIGN.md is agreed with the owner;
do not change those without asking.

## Current state (read this first)

The skeleton was written in a sandbox **without access to the Go module
proxy**, so third-party dependencies were never downloaded.

Verified there (`go vet` + `go test` pass, stdlib only):
- `pkg/api`, `pkg/client`, `pkg/ingressdns`
- `internal/zone`, `internal/cache`, `internal/config`, `internal/sdnotify`,
  `internal/metrics`, `internal/httpapi` (end-to-end with pkg/client, fake store)
- `internal/store` pure logic (`plan_test.go`)
- `cmd/horus` CLI code typechecked with `serve` stubbed out
- systemd units pass `systemd-analyze verify`; manifests are valid YAML

**Not compiled against the real dependencies:**
- `internal/dnsserver`, `cmd/horus` (serve) and all test files were
  typechecked (`go vet`) against a hand-written stub of the miekg/dns API
  surface they use. That catches our own mistakes, not mismatches with the
  real library, and none of their tests have run.
- `internal/store/store_sqlite_test.go` (modernc.org/sqlite) never ran.
- Everything under `operator/` (controller-runtime) is only syntax-checked.
Expect small fixes.

### First tasks, in order

1. `go mod tidy` in the repo root and in `operator/`.
2. `go vet ./... && go test -race ./...` in the root. Fix compile errors in
   `internal/dnsserver` and `cmd/horus` first, then make
   `internal/store/store_sqlite_test.go` and `internal/dnsserver/server_test.go`
   pass. Don't weaken tests to make them pass; fix the code unless the test is
   genuinely wrong (then say why).
3. `cd operator && go vet ./... && go test -race ./...`.
   `GetEventRecorderFor` may be deprecated in recent controller-runtime; keep it
   unless it no longer compiles.
4. Local smoke test without root: run `horus serve` with
   `HORUS_DNS_LISTEN=127.0.0.1:5353 HORUS_API_SOCKET=$PWD/api.sock HORUS_DB=$PWD/dev.db HORUS_ADMIN_TOKEN_FILE=<file with 40 chars>`,
   then `HORUS_ADDR=unix://$PWD/api.sock horus zone add ...` and
   `dig @127.0.0.1 -p 5353 ...`. Check forwarding, NXDOMAIN+SOA, CNAME chase.

## Commands

```sh
make tidy            # go mod tidy (root + operator)
make vet test        # root module
make operator-test   # operator module
make build operator  # bin/horus, bin/horus-operator
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

## Known gaps / next steps (not started)

- TLS for the API listener with `DynamicUser=yes`: cert/key readability
  (probably `LoadCredential=` for cert and key too). Document the chosen way.
- e2e test with kind: horus on the host, operator in kind, real Ingress with a
  fake LB status → `dig` resolves.
- `horus record ls` could filter by owner; `horus owners` admin view.
- Module path `github.com/warewave/horus` is a placeholder; change it in
  `go.mod`, `operator/go.mod` and imports if the repo lives elsewhere.
