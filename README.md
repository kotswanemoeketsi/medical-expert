# Medical Expert System — backend, rebuild stage 2 of 4

A multi-tenant clinical record system for South African practices, built on
Go and MySQL or MariaDB. Claude drafts consultation notes from a clinician's
dictation.

This is a rebuild of the original codebase, which was lost when its build
container reset. It is delivered in stages, and each stage runs and is
tested before it is handed over. `SYSTEM-DOCUMENTATION.md` (delivered
separately) describes the whole system; this README covers what is in this
archive.

| Stage | Contents | Status |
|---|---|---|
| 1 | Tenancy, sign-in with two-step verification, staff accounts, patient record, consent, audit | done |
| **2** | Medication and regimens, consultation notes with AI drafting, repeat prescriptions, lab results, referral letters and sick notes | **this archive** |
| 3 | WhatsApp notifications, practice dashboard, chronic recall lists, dose scheduling and maintenance jobs, demo data | next |
| 4 | React frontend on AdminLTE 3 | |

The git history in this archive has one commit per stage, so
`git show --stat HEAD` lists what stage 2 changed.

## Quick start

**Windows:** follow [`docs/WINDOWS.md`](docs/WINDOWS.md). It covers
installing Go and MySQL, PowerShell quoting, and the API in PowerShell, from
the first sign-in to sick notes and AI drafting.

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

**Upgrading from stage 1:** run `go run ./cmd/migrate`. It applies
migrations 0002 to 0005 and leaves your data alone.

**Optional:**
- **AI drafting.** Set `ANTHROPIC_API_KEY` in `.env`. Leave it unset until a
  data processing agreement with Anthropic is in place; drafting is then
  simply switched off.
- **Medication catalogue.** Load your authoritative list with
  `go run ./cmd/catalogue -import medicines.csv`. The file format is
  described at the top of `cmd/catalogue/main.go`. Comma- and
  semicolon-separated files both work, so a CSV saved from Excel imports
  as it is.

Requirements: Go 1.22 or later; MySQL 8 or MariaDB 10.6+. The only Go
dependency (`github.com/go-sql-driver/mysql`) downloads on the first build.

## Stage 1: identity, patients, consent, audit

**Practice separation (BR-0.3).**
- Data access code gets a database handle bound to one practice (a
  `tenant.Scope`) and can reach the database no other way.
- Every statement names the practice with a `:tenant` marker that the
  scope fills in; a statement without it is refused before it runs.
- Child rows reference their parent by `(tenant_id, id)`, so the database
  also refuses a row that points into another practice.
- Tests parse the source code and check every SQL statement. An unscoped
  query, SQL built at runtime, or a raw handle outside the two documented
  exceptions fails the build.

**Sign-in (M1).**
- Sessions can be revoked at once. Idle (30 min) and absolute (12 h)
  timeouts are enforced in the lookup itself.
- Passwords are hashed with PBKDF2 at 600,000 iterations.
- Every failed sign-in gets the same message and takes the same time, so
  nobody can find out which practices or emails exist.
- Accounts lock after five failures; there is also per-address rate
  limiting.
- Two-step verification uses an authenticator app. A password alone reaches
  nothing but the verify endpoint, and each code works once. There are ten
  recovery codes. Wrong codes count towards the lockout.

**Accounts, patients, consent, audit.**
- Doctors and nurses need a registration number.
- South African ID numbers are validated. Phone numbers are stored in
  international form.
- Duplicate patients trigger a prompt rather than a block.
- Allergies and conditions are never deleted.
- Consent is append-only, per purpose. No record means not granted.
- Every read of patient data is audited. If the audit entry cannot be
  written, the data is not returned.

## Stage 2: clinical work

**Medication and regimens (M4 core, BR-4.1, BR-4.10).**
- A shared catalogue ships with 32 common chronic medicines. Scheduling
  status and NAPPI codes are deliberately left blank; load the
  authoritative list with `cmd/catalogue`.
- Doctors start regimens. Pausing, stopping or completing one records who
  did it and why. Stopped and completed are final.
- Each scheduled dose records whether it was taken or skipped, and whether
  the patient answered through the portal or a staff member recorded it for
  them — the two are different evidence.
- Doses are scheduled by hand for now; generating them automatically from
  the regimen comes in stage 3.

**Consultation notes (BR-2.4, BR-2.7).**
- Draft → signed, one way. Only the author edits or signs a note.
- Saving a draft replaces its text and vitals with what was sent. The type
  and time are kept if they are left out, so a save never quietly moves a
  note to "consultation, now".
- A note needs an assessment or a plan before it can be signed.
- Vitals are range-checked to catch typing slips.
- A signed note changes only through amendments. Each keeps the previous
  text, who changed it, when and why, and the original signature stands.
- Only the five body fields can be amended.

**AI note drafting (M5, narrowed to transcription).**
- **A scribe only.** The model is told never to add clinical content,
  diagnose, recommend, or guess. It must answer through a tool that has
  only the five note fields and a list of things to check. It runs at
  temperature 0.
- **Consent first.** Nothing is sent without the patient's
  `ai_assisted_review` consent. The check happens before the model is
  called; the tests prove the model is never contacted.
- **Minimal data.** The patient's name and ID number are replaced with
  placeholders before sending. The background is labelled as background:
  age, sex, allergies, active conditions and current medication.
- **Never inside the note.** Each draft is stored beside the note, with the
  dictation. Any output outside the note fields is discarded and flagged.
  The note is marked as AI-assisted, permanently.
- **Controls.**
  - A per-practice daily limit (BR-5.18).
  - A two-minute time budget per draft.
  - Failed attempts are kept.
  - Clinicians can flag a draft as wrong or unsafe for review (BR-5.15).

**Repeat prescriptions.**
- Doctors only, and only for patients with an active chronic condition.
- A patient with no signed consultation within their review interval
  (default 6 months) needs an override reason, recorded on the script.
- Items are copied from the regimen, so later changes do not alter an issued
  script.
- Validity is at most 6 months, and a repeat never extends it.
- The repeat limit is enforced in the database. Two doctors clicking at once
  cannot both spend the last repeat; a test runs four at once to prove it.
- **No interaction, allergy or appropriateness checking, by design.** The
  response lists the recorded allergies and conditions for information,
  with an explicit notice that nothing was checked.

**Laboratory results.**
- Captured by hand. Values, units, ranges and flags are stored exactly as
  the laboratory printed them; nothing is derived.
- The practice inbox lists unacknowledged results, critical first, each with
  its analytes.
- A result is acknowledged once, with an optional note. The patient summary
  counts abnormal results nobody has looked at yet.

**Referral letters and medical certificates.**
- Clinicians draft; doctors issue.
- Issuing numbers the document per practice, type and year (`REF-2026-0001`,
  `MC-2026-0001`). The text is rendered once and frozen.
- **Certificates:**
  - They state whether they rest on the doctor's examination or on what the
    patient reported (HPCSA).
  - The diagnosis is withheld unless the patient agreed, and the certificate
    says so.
  - A backdated certificate needs a reason and says that it is backdated.
  - The ID number is left off, because an employer does not need it.
- **Referral letters** include medication and allergies only when the
  clinician asks for them.
- A withdrawn document keeps its number and text.

**Patient summary (BR-2.9)** now includes:
- current medication;
- the five most recent signed notes;
- how many draft notes are open;
- how many abnormal results are unacknowledged.

## API

Base path `/api/v1`. Every endpoint except `/healthz` and login needs
`Authorization: Bearer <token>`. Errors always look like:

```json
{"error": {"code": "validation_failed", "message": "Some fields need attention.",
           "fields": [{"field": "mobile", "message": "is required when reminders are sent by WhatsApp"}],
           "request_id": "3f2a9c1e5b7d4a60"}}
```

"Clinicians" means doctors and nurses; "staff" adds practice administrators.

| Method | Path | Who |
|---|---|---|
| GET | `/healthz` | public |
| POST | `/auth/login` | public (rate limited) |
| POST | `/auth/logout` | any session |
| GET | `/me` | signed in (or enrolling) |
| POST | `/auth/password` | signed in (or enrolling) |
| POST | `/auth/mfa/verify` | password-only session |
| GET | `/auth/mfa/status` | signed in (or enrolling) |
| POST | `/auth/mfa/enrol`, `/auth/mfa/enrol/confirm` | signed in (or enrolling) |
| POST | `/auth/mfa/recovery-codes` | signed in; needs a current code |
| POST, GET | `/users` | practice admin |
| PATCH | `/users/{id}/status` | practice admin (setting active also unlocks) |
| POST | `/users/{id}/mfa/reset` | practice admin |
| GET | `/audit?subject_id=&actor_id=&action=&since=&before=&limit=` | practice admin |
| POST | `/patients` | staff |
| GET | `/patients?q=&limit=&offset=` | staff |
| GET | `/patients/{id}` | staff, or that patient |
| PUT | `/patients/{id}` | staff |
| GET | `/patients/{id}/summary` | clinicians, or that patient |
| POST | `/patients/{id}/allergies`, `/patients/{id}/conditions` | clinicians |
| PATCH | `/patients/{id}/allergies/{allergyId}`, `/patients/{id}/conditions/{conditionId}` | clinicians |
| POST | `/patients/{id}/consents` | staff |
| GET | `/patients/{id}/consents` | staff, or that patient |
| PATCH | `/patients/{id}/review-interval` | clinicians |
| GET | `/medications?q=` | staff |
| POST | `/patients/{id}/regimens` | doctors |
| GET | `/patients/{id}/regimens` | clinicians, or that patient |
| POST | `/regimens/{id}/status` | doctors |
| POST | `/regimens/{id}/doses` | clinicians |
| GET | `/patients/{id}/doses?from=&to=` | clinicians, or that patient |
| POST | `/doses/{id}/response` | clinicians, or that patient |
| POST | `/patients/{id}/encounters` | clinicians |
| GET | `/patients/{id}/encounters` | clinicians (patients see signed notes only) |
| GET | `/encounters/{id}` | clinicians |
| PATCH | `/encounters/{id}` | the author, while a draft |
| POST | `/encounters/{id}/sign` | the author |
| POST | `/encounters/{id}/amendments` | clinicians, once signed |
| POST | `/encounters/{id}/draft-note` | the author, while a draft |
| GET | `/encounters/{id}/drafts` | clinicians |
| POST | `/encounters/{id}/drafts/{draftId}/flag` | clinicians |
| POST | `/patients/{id}/prescriptions` | doctors |
| GET | `/patients/{id}/prescriptions`, `/prescriptions/{id}` | clinicians |
| POST | `/prescriptions/{id}/repeat`, `/prescriptions/{id}/cancel` | doctors |
| POST | `/patients/{id}/lab-results` | clinicians |
| GET | `/patients/{id}/lab-results`, `/lab-results/{id}` | clinicians |
| GET | `/lab-results?abnormal_only=&limit=` (the inbox) | clinicians |
| POST | `/lab-results/{id}/acknowledge` | clinicians |
| POST | `/patients/{id}/referrals`, `/patients/{id}/sick-notes` | clinicians |
| GET | `/patients/{id}/documents`, `/documents/{id}` | clinicians |
| POST | `/documents/{id}/issue` | doctors |
| POST | `/documents/{id}/cancel` | clinicians |

Notable error codes:
- **Sign-in:** `invalid_credentials`, `mfa_required`, `mfa_setup_required`,
  `invalid_code`, `rate_limited` (429, with `Retry-After`).
- **Patients and accounts:** `possible_duplicate` (409, with `candidates`
  and `overridable`), `national_id_exists`, `email_taken`, `self_action`.
- **Notes and AI drafting:** `not_author`, `note_state`,
  `ai_consent_required`, `ai_unavailable` (503), `ai_failed` (503),
  `ai_limit_reached` (429).
- **Prescriptions:** `not_chronic`, `review_overdue` (with `last_review`),
  `repeats_exhausted`, `prescription_expired`, `prescription_cancelled`.
- **Labs, documents and doses:** `already_acknowledged`,
  `backdate_reason_required`, `document_state`, `dose_state`,
  `dose_not_due`, `regimen_state`.

## Tests

```bash
go test ./...                                                  # 99 unit tests, no database
MES_TEST_DSN='mes:pw@tcp(127.0.0.1:3306)/mes_test' \
  go test -tags=integration ./...                              # all 149
```

**Where it has run.**
- The suite passes on Go 1.22 (with the race detector) and Go 1.24,
  against MariaDB 10.11.
- MySQL 8 is supported by design, but was not available in the build
  environment. If MySQL 8 is your target, run the integration suite
  against it once.

**What the integration tests cover.** They exercise the real HTTP stack
against a real database, including:
- a password-only session tried against every route;
- a valid session from one practice tried against every read and write path
  of another (stage 1 and stage 2 alike);
- the AI consent gate, with a stand-in drafter;
- four doctors racing for the last repeat;
- document numbering under concurrency.

**Checked by breaking the code on purpose.** Each of these deliberate bugs
made the suite fail:
- an unscoped query;
- SQL built at runtime;
- a raw database handle in a scoped repository;
- the MFA gate switched off;
- a patient reading another patient's record;
- the password stage resetting the failure count;
- the AI consent check removed;
- name redaction removed;
- AI provenance not recorded;
- the repeat bound removed from the SQL;
- a diagnosis printed without consent;
- the lab inbox not sorting critical results first;
- a list endpoint answering "empty" instead of "not found" for another
  practice's patient;
- the review override not being required;
- saving a draft resetting the note's type and time.

The real server was also run end to end: upgrading a stage 1 database,
importing the catalogue, and drafting a note through the real client
against a stand-in provider. That run confirmed the request had
temperature 0, a forced tool, and no name, ID number or phone number in
the payload.

Every PowerShell example in `docs/WINDOWS.md` was then run, in order,
against a fresh database and a live server. The only error was the
duplicate-patient warning the guide sets out to show. This used PowerShell
7; Windows PowerShell 5.1 was not available.

## Configuration

All settings are environment variables, optionally in a `.env` file — see
[`.env.example`](.env.example).

New in stage 2:

| Variable | Default | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | — | Unset: drafting is off |
| `MES_AI_MODEL` | `claude-sonnet-5-5` | |
| `MES_AI_BASE_URL` | `https://api.anthropic.com` | Must be https in production |
| `MES_AI_TIMEOUT` | `90s` | Per request; each draft has a two-minute budget overall |
| `MES_AI_DAILY_LIMIT` | `200` | Drafting requests per practice per day, failures included |
| `MES_DEFAULT_REVIEW_MONTHS` | `6` | Review interval for patients without their own |

With `MES_ENV=production` the server refuses to start if:
- MFA is off;
- sessions last longer than 24 hours or idle longer than an hour;
- the database account is `root`;
- drafting would send dictations over plain http.

## Layout

```
cmd/server           API server; hourly jobs: session clean-up, script expiry
cmd/migrate          applies embedded migrations; -check exits 2 if any are pending
cmd/bootstrap        creates a practice and its first administrator
cmd/catalogue        imports medicines into the shared catalogue from CSV
internal/tenant      the isolation primitive everything rests on
internal/model       entities and invariants
internal/secure      PBKDF2, session tokens, TOTP, recovery codes
internal/phone       E.164 normalisation and display
internal/ai          note drafting: the scribe prompt, the tool schema, the API client
internal/document    referral letter and certificate rendering
internal/db          connection set-up, embedded migrations and the runner
internal/repository  data access; every type built from a tenant.Scope
internal/service     every business rule; never imports net/http
internal/controller  HTTP handlers, middleware, routing, rate limiting
internal/config      environment configuration and production checks
internal/job         scheduled maintenance
db/migrations        readable copy of the schema (kept identical by a test)
db/setup.sql         creates the databases and the application account
```

## Before real patient data

- **TLS.** Terminate it at a reverse proxy. HSTS is already sent in
  production. Set `MES_TRUST_PROXY_HEADERS=true` only behind that proxy.
- **AI.** Sign a data processing agreement with Anthropic before setting
  `ANTHROPIC_API_KEY` (BR-5.16, REG-07). Record AI consent per patient.
- **Database accounts.** Use a separate migration account, and make
  `audit_log` append-only with grants (see `db/setup.sql`).
- **Backups.** Take them, and test a restore.
- **Compliance.**
  - Complete the POPIA DPIA and appoint an Information Officer.
  - Get a regulatory opinion on device classification before AI drafting
    goes live.
  - Have a clinician review the certificate and referral wording against
    your practice's requirements.

## Rebuild notes: where this differs from the original

**Stage 1**
- The MFA pieces are part of `0001_init.sql`.
- Child tables carry composite `(tenant_id, id)` foreign keys.
- Recovery codes are 80 bits long.
- Completing the second factor issues a new token.
- Administrators can register patients but cannot read clinical detail.
- `.env` files are read automatically.
- Time-zone data is embedded in the binary.

**Stage 2**
- Regimens and dose records arrive in this stage, because prescriptions and
  AI drafting depend on them.
- The medication catalogue is read through the platform repository, which
  is one of the two documented exceptions to practice scoping: it is shared
  reference data, not practice data.
- Drafting now:
  - replaces names with placeholders before sending;
  - discards and flags any output outside the note fields;
  - lets clinicians flag drafts for review;
  - applies a daily limit per practice.
- The default model is `claude-sonnet-5-5`, configurable.
- New endpoints:
  - `POST /regimens/{id}/status`;
  - `GET /patients/{id}/doses`;
  - `GET /medications`;
  - draft flagging;
  - the `cmd/catalogue` import tool.
- Certificates leave out the ID number. Phone numbers print the way South
  Africans write them.
