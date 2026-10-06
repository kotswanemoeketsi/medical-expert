# Medical Expert System — backend, rebuild stage 3 of 4

A multi-tenant clinical record system for South African practices, built on
Go and MySQL or MariaDB. Claude drafts consultation notes from a clinician's
dictation, and patients are reminded to take their medicine on WhatsApp.

This is a rebuild of the original codebase, which was lost when its build
container reset. It is delivered in stages, and each stage runs and is
tested before it is handed over. `SYSTEM-DOCUMENTATION.md` (delivered
separately) describes the whole system; this README covers what is in this
archive.

| Stage | Contents | Status |
|---|---|---|
| 1 | Tenancy, sign-in with two-step verification, staff accounts, patient record, consent, audit | done |
| 2 | Medication and regimens, consultation notes with AI drafting, repeat prescriptions, lab results, referral letters and sick notes | done |
| **3** | WhatsApp reminders and recalls, dose scheduling, practice dashboard, chronic recall lists, maintenance jobs, demo data | **this archive** |
| 4 | React frontend on AdminLTE 3 | next |

The git history in this archive has one commit per stage, so
`git show --stat HEAD` lists what stage 3 changed.

## Quick start

**Windows:** follow [`docs/WINDOWS.md`](docs/WINDOWS.md). It covers
installing Go and MySQL, PowerShell quoting, and the API in PowerShell, from
the first sign-in to sick notes, AI drafting, the dashboard and recall lists.

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

**A practice full of history to look at:** `go run ./cmd/seed` builds a demo
practice (`demo`) of 24 made-up patients with two months of reminders,
replies, notes, scripts and results, and prints the sign-ins. It takes about
half a minute. See [Demo data](#demo-data).

**Upgrading from stage 1 or 2:** run `go run ./cmd/migrate`. It applies the
migrations you do not have yet (0002 to 0006) and leaves your data alone.

**Optional:**
- **AI drafting.** Set `ANTHROPIC_API_KEY` in `.env`. Leave it unset until a
  data processing agreement with Anthropic is in place; drafting is then
  simply switched off.
- **Medication catalogue.** Load your authoritative list with
  `go run ./cmd/catalogue -import medicines.csv`. The file format is
  described at the top of `cmd/catalogue/main.go`. Comma- and
  semicolon-separated files both work, so a CSV saved from Excel imports
  as it is.
- **WhatsApp.** Set the four `MES_WHATSAPP_*` variables for a WhatsApp
  Business account, get the three message templates approved, and switch
  messaging on per practice. [`docs/WHATSAPP.md`](docs/WHATSAPP.md) walks
  through it. Without it, everything else works and nothing is sent.

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
- Doses can be added by hand for a schedule the frequencies do not cover.
  Stage 3 generates the usual ones.

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

## Stage 3: reminders, the dashboard, recall lists

**Dose scheduling (BR-4.2, BR-4.10).**
- Doses are generated from each regimen's frequency and the patient's
  waking hours, in the practice's time zone, a week ahead. With the default
  hours (07:00 to 21:00) twice daily means 08:00 and 20:00, and four times
  daily 08:00, 12:00, 16:00 and 20:00. Night-shift hours such as 20:00 to
  08:00 work. Every-other-day and weekly medicines count from the start
  date; as-needed medicines have no schedule.
- **Only patients who can answer get a schedule:** those with their own
  login, or those the practice can message on WhatsApp with their consent.
  A dose nobody can answer is closed as missed when its window ends, so
  scheduling one for a patient who was never asked would invent
  non-adherence.
- The schedule follows changes at once. Pausing or stopping a regimen
  withdraws its future doses in the same transaction, including any added
  by hand: nobody should be reminded about a medicine they were told to
  stop. A new regimen, changed waking hours, new consent or a new login
  reschedules the patient straight away. The hourly run catches anything
  else.
- Rescheduling never moves or removes a dose added by hand, or one already
  reminded or answered.

**WhatsApp reminders (BR-4.3, BR-4.5, M7).**
- Nothing is sent until the server has a WhatsApp account and an
  administrator switches messaging on for the practice. A demo practice
  never sends, whatever the configuration.
- Each minute, every dose that has come due gets a reminder. One message
  covers every dose a patient has at the same time: "Hi Thabo, it's time
  for your 08:00 medicine. Reply TAKEN once you've taken it, or SKIP if you
  won't be taking it. Reply STOP to stop these reminders."
- **No clinical detail leaves the building.** Messages carry a first name,
  a time and the practice's name, never a medicine, condition or result.
  Every message is checked against a list of health words and every name
  in the medication catalogue, down to each medicine of a combination
  product, before it goes out. A practice whose name
  says what it treats sets a neutral messaging name before messaging can be
  switched on.
- The notification is written before the send, so a failure mid-flight is
  still on record. Rate limits and provider outages are retried twice, with
  a lease so two servers never send the same message. A number that is not
  on WhatsApp fails once and is recorded; no follow-up is sent to it.
- One follow-up goes out an hour later (configurable per practice) if the
  doses are still unanswered. A reminder that cannot go out within an hour
  of its time is not sent at all.
- Each practice has a daily message limit (BR-0.8).
- Clinicians can send a reminder by hand, and any staff member a recall
  ("please phone to book a check-up"). Recalls are limited to one a week
  per patient.

**Patients' replies.**
- The Taken and Skip buttons carry a random token that identifies the
  reminder, so a button press answers exactly the doses it asked about.
  Typed replies are matched to the message they reply to, or else to the
  latest message sent to that number.
- Only whole answers are read: TAKEN, SKIP, STOP and a few close variants.
  Anything else, such as "I'm not sure, can I take it later?", changes
  nothing and is kept for staff to read. An early version of the original
  read the word "not" as a skip.
- STOP withdraws consent to reminders, recorded as the patient's own
  decision, and their future doses are withdrawn.
- A reply from a different number than the reminder went to is ignored. A
  webhook delivered twice is applied once.
- The webhook checks Meta's signature on the raw body before reading
  anything. With no app secret configured, every webhook is refused.
- Delivery receipts only move a message forward: sent, delivered, read.

**Dashboard (BR-9.1).** Counts only, with nothing that identifies a patient,
because it is the kind of screen left open at reception:
- active and chronic patients, and patients per chronic condition (the
  payload says why the rows add up to more than the total);
- patients seen today and in the period; open draft notes;
- response rate and adherence rate, kept apart: a patient who reliably
  answers "skipped" is fully responsive and taking nothing;
- patients grouped by how often they answer (none, 1 to 49%, 50 to 79%,
  80 to 99%, every dose), because averages hide who to phone;
- unacknowledged, abnormal and critical results; scripts about to expire or
  out of repeats; messages in the last week;
- notes on what each figure measures.

Pending doses, whose answer window is still open, are left out of every
rate.

**Recall lists.** Five worklists, all limited to active patients with an
active chronic condition, with names and phone numbers because the point is
that somebody phones:

| Reason | Finds |
|---|---|
| `review_overdue` | No signed consultation within the patient's review interval, or none ever. Drafts do not count. |
| `script_expiring` | The latest script runs out within 14 days (`days=`) |
| `repeats_exhausted` | The latest script is valid but has no repeats left |
| `not_responding` | Answered under 50% (`threshold=`) of at least 4 (`min_doses=`) doses in 30 days (`days=`) |
| `unacknowledged_result` | Abnormal results nobody has reviewed for a day. Clinicians only. |

A newer script takes a patient off the script lists. Every pull is audited
as a read of patient data, with the patients it listed.

**Adherence per medicine (BR-4.6).** The patient summary gives each current
medicine's taken, skipped and missed counts, response rate and adherence
rate over 7, 30 and 90 days.

**Maintenance jobs.** The server runs these hourly, per practice, recovering
from panics:
- schedule doses a week ahead;
- close unanswered doses as missed once their window ends (12 hours by
  default, `MES_DOSE_GRACE`), because unanswered doses stay pending and
  pending doses are left out of adherence, so without this a patient who
  ignores every reminder looks like one whose doses are not due yet;
- expire lapsed scripts; purge ended sessions.

Reminders are sent by a separate job every minute (NFR-05).

## Demo data

```bash
go run ./cmd/seed -slug demo -name "Greenside Clinic"
```

This builds a practice of 24 made-up South African patients by running the
real services on a simulated clock. The patient records go back almost a
year: registrations, medicines, reviews, scripts and repeats, results,
referral letters and sick notes. For the last 60 days (`-days`), the real
scheduler and dispatcher run against an in-process stand-in for WhatsApp.
Simulated patients answer the way real ones do: some reliably, some
patchily, some never. One skips often, one number is not on WhatsApp, and
two patients answer in the app. The dashboard, recall lists and message
histories therefore have something real to show.

The command prints a sign-in for the administrator, a doctor, a nurse and
two patients. `-password` sets one password for all of them. `-seed`
changes the random history. The practice is marked as a demo: its names,
ID numbers and phone numbers are invented, and it never sends a message.

## API

Base path `/api/v1`. Every endpoint except `/healthz`, login and the
WhatsApp webhook needs `Authorization: Bearer <token>`. Errors always look
like:

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
| GET | `/practice` | staff |
| PATCH | `/practice` (details, `whatsapp_enabled`, `messaging_name`, `follow_up_minutes`) | practice admin |
| GET | `/dashboard?days=` | staff |
| GET | `/recalls?reason=&days=&threshold=&min_doses=&limit=` | staff (`unacknowledged_result`: clinicians) |
| POST | `/patients/{id}/notifications/reminder` | clinicians |
| POST | `/patients/{id}/notifications/recall` | staff |
| GET | `/patients/{id}/notifications` (messages sent and replies) | clinicians |
| GET | `/notifications/health` | staff |
| POST | `/maintenance/close-overdue-doses` | practice admin |
| GET | `/webhooks/whatsapp` (Meta's subscription check) | public, verify token |
| POST | `/webhooks/whatsapp` (receipts and replies) | public, signed |

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
- **Messages:** `messaging_not_configured` (503), `messaging_off`,
  `demo_practice`, `comms_consent_required`, `no_whatsapp` (the message
  says why: no number, prefers SMS, asked not to be contacted),
  `patient_inactive`, `nothing_due`, `recently_recalled` (with
  `last_recall`), `practice_phone_missing`, `clinical_detail` (422),
  `notification_limit_reached` (429).

## Tests

```bash
go test ./...                                                  # 119 unit tests, no database
MES_TEST_DSN='mes:pw@tcp(127.0.0.1:3306)/mes_test' \
  go test -tags=integration ./...                              # all 186
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
- document numbering under concurrency;
- WhatsApp end to end: the real client talks to a stand-in Graph API, so
  the tests read exactly the JSON that would leave the building, and signed
  webhooks carry receipts and replies back;
- four dispatchers racing for the same doses;
- the scheduler following consent, logins, waking hours, pauses and the
  practice's messaging switch;
- a dashboard and recall lists over a history built with a moving clock;
- the demo seeder, run in full.

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
- saving a draft resetting the note's type and time;
- reading a reply by searching for words inside it ("I'm not sure" became
  a skip);
- the webhook signature check skipped when no app secret is set;
- the reminder claim not required, so two dispatchers could both remind;
- follow-ups sent to numbers whose first reminder failed;
- doses scheduled for patients nobody can ask;
- skipped doses counted as unanswered in the response rate;
- the review-overdue list counting draft notes;
- a medicine's name allowed into a message, whether the check skipped the
  catalogue or the catalogue was never loaded.

The real server was also run end to end: upgrading a stage 1 database,
importing the catalogue, and drafting a note through the real client
against a stand-in provider. That run confirmed the request had
temperature 0, a forced tool, and no name, ID number or phone number in
the payload.

Every PowerShell example in `docs/WINDOWS.md` was then run, in order,
against a fresh database and a live server. The only error was the
duplicate-patient warning the guide sets out to show. This used PowerShell
7; Windows PowerShell 5.1 was not available.

For stage 3 the real server binary also ran against a stand-in Graph API,
with reminders going out on the minute, and signed webhooks delivering
button presses, typed replies, STOP and receipts. The demo seeder's
practice was then served and its dashboard and recall lists read back. A
stage 2 database holding patients, consent and answered doses was upgraded
with `cmd/migrate` and served by the stage 3 server: the doses kept their
answers and were marked as added by hand.

## Configuration

All settings are environment variables, optionally in a `.env` file — see
[`.env.example`](.env.example).

Stage 2:

| Variable | Default | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | — | Unset: drafting is off |
| `MES_AI_MODEL` | `claude-sonnet-5-5` | |
| `MES_AI_BASE_URL` | `https://api.anthropic.com` | Must be https in production |
| `MES_AI_TIMEOUT` | `90s` | Per request; each draft has a two-minute budget overall |
| `MES_AI_DAILY_LIMIT` | `200` | Drafting requests per practice per day, failures included |
| `MES_DEFAULT_REVIEW_MONTHS` | `6` | Review interval for patients without their own |

Stage 3:

| Variable | Default | Notes |
|---|---|---|
| `MES_WHATSAPP_PHONE_NUMBER_ID` | — | With the access token: messaging available. Unset: nothing is ever sent |
| `MES_WHATSAPP_ACCESS_TOKEN` | — | A system user's permanent token |
| `MES_WHATSAPP_APP_SECRET` | — | Checks webhook signatures. Unset: every webhook is refused |
| `MES_WHATSAPP_VERIFY_TOKEN` | — | Any secret you choose; Meta echoes it when the webhook is set up |
| `MES_WHATSAPP_API_VERSION` | `v23.0` | Graph API version |
| `MES_WHATSAPP_BASE_URL` | `https://graph.facebook.com` | Must be https in production |
| `MES_WHATSAPP_LANGUAGE` | `en` | Language code the templates were approved under |
| `MES_REMINDER_FOLLOW_UP` | `60m` | Follow-up delay, 10m to 12h, or `off`. Practices can set their own |
| `MES_DOSE_GRACE` | `12h` | How long a dose stays answerable before it is closed as missed |
| `MES_NOTIFICATION_DAILY_LIMIT` | `1000` | Messages per practice per day |

With `MES_ENV=production` the server refuses to start if:
- MFA is off;
- sessions last longer than 24 hours or idle longer than an hour;
- the database account is `root`;
- drafting would send dictations over plain http;
- WhatsApp is configured without an app secret, or over plain http.

## Layout

```
cmd/server           API server; hourly jobs (sessions, scripts, doses) and the reminder dispatcher
cmd/migrate          applies embedded migrations; -check exits 2 if any are pending
cmd/bootstrap        creates a practice and its first administrator
cmd/catalogue        imports medicines into the shared catalogue from CSV
cmd/seed             builds a demo practice by running the system on a simulated clock
internal/tenant      the isolation primitive everything rests on
internal/model       entities and invariants
internal/secure      PBKDF2, session tokens, TOTP, recovery codes
internal/phone       E.164 normalisation and display
internal/ai          note drafting: the scribe prompt, the tool schema, the API client
internal/notify      WhatsApp: templates, the clinical-detail check, the Cloud API client, webhooks, replies
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
- **WhatsApp.** Meta processes patients' names and numbers: cover it in your
  POPIA assessment and agreements (REG-07). Get the three templates
  approved (docs/WHATSAPP.md). Tell patients, when they consent, that
  reminders are an aid and may not arrive (BR-4.12).
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

**Stage 3**
- The original sent reminders only when asked, and had no scheduler; both
  are built here, with doses scheduled only for patients who can answer.
- One reminder covers all of a patient's doses at one time, and its buttons
  carry a token, so a reply answers exactly what was asked. The original
  applied a reply to the most recent unanswered dose.
- STOP withdraws consent (BR-7.5), so `patient_consents.recorded_by` may
  now be empty, meaning the patient did it themselves.
- Unreadable replies are kept for staff instead of being dropped.
- Practices switch messaging on themselves (`PATCH /practice`), can set a
  messaging name and follow-up delay, and have a daily limit.
- Per-medicine adherence over 7, 30 and 90 days is in the patient summary.
- `unacknowledged_result` recalls are for clinicians only.
- The migrations are numbered differently from the original's; the schema
  is the same in substance.
