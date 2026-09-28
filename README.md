# Medical Expert System — backend, rebuild stage 1 of 4

A multi-tenant clinical record system for South African practices: Go, MySQL
or MariaDB, and (from stage 2) Claude for consultation-note drafting.

This is a rebuild of the original codebase, which was lost when its build
container reset. It is delivered in stages so each piece runs and is tested
as it lands. `SYSTEM-DOCUMENTATION.md` (delivered separately) describes the
whole system; this README covers what is in this archive.

| Stage | Contents | Status |
|---|---|---|
| **1** | Tenancy, sign-in with two-step verification, staff accounts, patient record, consent, audit | **this archive** |
| 2 | Consultation notes and AI drafting, prescriptions and repeats, lab results, referral letters and sick notes | next |
| 3 | WhatsApp notifications, practice dashboard, chronic recall lists, maintenance jobs, demo data | |
| 4 | React frontend on AdminLTE 3 | |

## Quick start

**Windows:** follow [`docs/WINDOWS.md`](docs/WINDOWS.md) — it covers
installing Go and MySQL, PowerShell quoting, and trying the API.

**Linux / macOS:**

```bash
# 1. database and account (edit the password in the file first)
mysql -u root -p < db/setup.sql

# 2. configuration
cp .env.example .env        # set the password in MES_DSN

# 3. schema, a practice, and the server
go run ./cmd/migrate
go run ./cmd/bootstrap -slug greenside -name "Greenside Clinic" \
  -admin-email admin@greenside.co.za -admin-name "Jane Admin"
go run ./cmd/server         # http://localhost:8080
```

`bootstrap` prints the administrator's password once. Sign in with the
practice code (`greenside`), the email and that password:

```bash
curl -s localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"tenant_slug":"greenside","email":"admin@greenside.co.za","password":"..."}'
```

Requirements: Go 1.22 or later; MySQL 8 or MariaDB 10.6+. The one Go
dependency (`github.com/go-sql-driver/mysql`) downloads on the first build.

## What this stage does

**Tenant isolation (BR-0.3).** Repositories are built from a `tenant.Scope`
whose database handle is unexported. Every statement names the tenant with a
`:tenant` marker that the scope binds itself; a statement without it is
refused before it reaches the driver. Child rows reference their parent by
`(tenant_id, id)`, so the database also refuses a row that points into
another practice. Tests parse the repository package and run every SQL
statement through the same guard, so an unscoped query fails the build.

**Sign-in (M1).**
- Opaque, revocable sessions stored as a SHA-256 digest; idle (30 min) and
  absolute (12 h) timeouts, revocation, and user and practice status are all
  checked in the lookup query.
- PBKDF2-HMAC-SHA256 at 600,000 iterations; hashes upgrade on sign-in if the
  work factor rises. Password policy blocks common words and the user's own
  name.
- One failure message for every reason a sign-in fails, and equal timing for
  unknown accounts, so nobody can enumerate practices or emails.
- Lockout after five failures (BR-1.6), plus per-address rate limiting. An
  administrator can unlock an account early by setting it active.
- TOTP two-step verification (RFC 6238, tested against the RFC vectors):
  - A password-only session can reach two endpoints and nothing else.
  - Each code works once, enforced in the SQL `WHERE` clause.
  - Ten single-use recovery codes.
  - Wrong codes count towards the lockout, and signing in again does not
    reset the count.
  - Completing the second factor issues a new token rather than upgrading
    the old one.
  - Required for staff roles in production (BR-1.3).
- Suspension and MFA resets revoke every session immediately (BR-1.7).

**Staff accounts (BR-1.4, BR-1.5).** Created by a practice administrator.
Doctors and nurses cannot be created without a registration number (HPCSA or
SANC). Patient logins are tied to exactly one patient record.

**Patient record (M2).**
- Demographics, next of kin, lifestyle and history fields.
- Validation for South African ID numbers: checksum, and date of birth against
  the number.
- Phone numbers stored in E.164 (`082 123 4567` → `+27821234567`).
- A mobile number is required when reminders go by WhatsApp or SMS.
- Allergies (severity shown most severe first) and conditions (ICD-10, with a
  chronic flag the clinician sets). Neither is ever deleted: entries are
  resolved or withdrawn as entered in error.
- Duplicate detection (BR-2.5) prompts rather than blocks, except for a
  matching ID number, which is the same person.

**Consent (BR-2.8).** Append-only, per purpose (data processing, AI-assisted
review, reminder communications). No record means not granted.

**Audit (BR-8.1).** Every sign-in event, administrative action and read of
patient data is recorded with the actor, IP and request ID. Reads fail closed:
if the access cannot be recorded, the data is not returned. Administrators can
read their own practice's trail.

**Access rules.**
- Practice administrators manage staff and register patients, but do not see
  clinical detail.
- Patients see only their own record; anyone else's answers 404, not 403.
- The platform operator role has no routine access to practice data.

## API

Base path `/api/v1`. Every endpoint except `/healthz` and login needs
`Authorization: Bearer <token>`. Errors always look like:

```json
{"error": {"code": "validation_failed", "message": "Some fields need attention.",
           "fields": [{"field": "mobile", "message": "is required when reminders are sent by WhatsApp"}],
           "request_id": "3f2a9c1e5b7d4a60"}}
```

| Method | Path | Who |
|---|---|---|
| GET | `/healthz` | public |
| POST | `/auth/login` | public (rate limited) |
| POST | `/auth/logout` | any session |
| GET | `/me` | signed in (or enrolling) |
| POST | `/auth/password` | signed in (or enrolling) |
| POST | `/auth/mfa/verify` | password-only session |
| GET | `/auth/mfa/status` | signed in (or enrolling) |
| POST | `/auth/mfa/enrol` | signed in (or enrolling) |
| POST | `/auth/mfa/enrol/confirm` | signed in (or enrolling) |
| POST | `/auth/mfa/recovery-codes` | signed in; needs a current code |
| POST, GET | `/users` | practice admin |
| PATCH | `/users/{id}/status` | practice admin |
| POST | `/users/{id}/mfa/reset` | practice admin |
| GET | `/audit?subject_id=&actor_id=&action=&since=&before=&limit=` | practice admin |
| POST | `/patients` | staff |
| GET | `/patients?q=&limit=&offset=` | staff |
| GET | `/patients/{id}` | staff, or that patient |
| PUT | `/patients/{id}` | staff |
| GET | `/patients/{id}/summary` | clinicians, or that patient |
| POST | `/patients/{id}/allergies` | clinicians |
| PATCH | `/patients/{id}/allergies/{allergyId}` | clinicians |
| POST | `/patients/{id}/conditions` | clinicians |
| PATCH | `/patients/{id}/conditions/{conditionId}` | clinicians |
| POST | `/patients/{id}/consents` | staff |
| GET | `/patients/{id}/consents` | staff, or that patient |
| PATCH | `/patients/{id}/review-interval` | clinicians |

Notable error codes: `invalid_credentials`, `mfa_required`,
`mfa_setup_required`, `invalid_code`, `possible_duplicate` (409, with
`candidates` and `overridable`), `national_id_exists`, `email_taken`,
`self_action`, `rate_limited` (429, with `Retry-After`).

## Tests

```bash
go test ./...                                                  # 73 unit tests, no database
MES_TEST_DSN='mes:pw@tcp(127.0.0.1:3306)/mes_test' \
  go test -tags=integration ./...                              # all 105
```

The suite passes on Go 1.22 (with the race detector) and Go 1.24, against
MariaDB 10.11. MySQL 8 is supported by design — the schema avoids anything
MariaDB-specific — but was not available in the build environment, so run
the integration suite once against it if that is your target. The
integration tests exercise the real HTTP stack against a
real database, including:
- a password-only session tried against every route;
- a valid session from one practice tried against every read and write path
  of another;
- the replay guard, recovery codes, lockout across repeated sign-ins,
  suspension and MFA reset.

Some tests were checked by breaking the code on purpose. Each of these
deliberate bugs made the suite fail:
- an unscoped query;
- SQL built at runtime;
- a raw database handle in a scoped repository;
- the MFA gate switched off;
- a patient reading another patient's record;
- the password stage resetting the failure count.

## Configuration

All settings are environment variables, optionally in a `.env` file — see
[`.env.example`](.env.example). With `MES_ENV=production` the server refuses
to start if:
- MFA is off;
- sessions last longer than 24 hours or idle longer than an hour;
- the database account is `root`.

## Layout

```
cmd/server        API server; runs the session-cleanup job hourly
cmd/migrate       applies embedded migrations; -check exits 2 if any are pending
cmd/bootstrap     creates a practice and its first administrator
internal/tenant   the isolation primitive everything rests on
internal/model    entities and invariants (IDs, dates, SA ID numbers)
internal/secure   PBKDF2, session tokens, TOTP, recovery codes
internal/phone    E.164 normalisation
internal/db       connection set-up, embedded migrations and the runner
internal/repository  data access; every type built from a tenant.Scope
internal/service  every business rule; never imports net/http
internal/controller  HTTP handlers, middleware, routing, rate limiting
internal/config   environment configuration and production checks
internal/job      scheduled maintenance
db/migrations     readable copy of the schema (kept identical by a test)
db/setup.sql      creates the databases and the application account
```

## Before real patient data

- **TLS.** Terminate it at a reverse proxy. HSTS is already sent in
  production. Set `MES_TRUST_PROXY_HEADERS=true` only behind that proxy.
- **Database accounts.** Use a separate migration account. Make `audit_log`
  append-only with grants (see `db/setup.sql`).
- **Backups.** Take them, and test a restore.
- **Compliance.** Complete the POPIA DPIA, appoint an Information Officer,
  and get a regulatory opinion on device classification before AI features
  go live.

## Rebuild notes: where this differs from the original

- The MFA columns and recovery codes are part of `0001_init.sql` rather than
  a later migration, and child tables carry composite `(tenant_id, id)`
  foreign keys.
- Recovery codes are 16 characters (80 bits); completing the second factor
  rotates the session token.
- New endpoints: `PUT /patients/{id}`, `POST /auth/password`,
  `GET /audit`, and status changes for allergies and conditions.
- Practice administrators may register patients and record consent, but not
  read clinical detail.
- `.env` files are read automatically.
- Time-zone data is embedded, so Windows hosts need nothing extra.
