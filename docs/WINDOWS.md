# Running on Windows

Everything here works in Windows PowerShell 5.1 and PowerShell 7. Commands
are run from the project folder (the one containing `go.mod`).

## 1. Install the tools

| Tool | Where | Check |
|---|---|---|
| Go 1.22 or later | https://go.dev/dl/ (the `.msi` installer) | `go version` |
| MySQL 8 **or** MariaDB 10.6+ | MySQL Installer for Windows, or the MariaDB `.msi` | `mysql --version` |
| Git (optional) | https://git-scm.com/download/win | `git --version` |

Open a **new** terminal after installing so `go` and `mysql` are on `PATH`.
If `mysql` is not found, it is usually in
`C:\Program Files\MySQL\MySQL Server 8.0\bin` or
`C:\Program Files\MariaDB 11.x\bin`; add that folder to `PATH` or call it by
its full path.

## 2. Unzip and unblock

Unzip the archive, open PowerShell in the extracted `medical-expert-system`
folder, and clear the "downloaded from the internet" mark so PowerShell will
run `make.ps1`:

```powershell
Get-ChildItem -Recurse | Unblock-File
```

## 3. Create the database and the application account

Edit `db\setup.sql` and change `change-this-password`. Then:

```powershell
Get-Content db\setup.sql | mysql -u root -p
```

PowerShell has no `<` redirection, which is why the file is piped. In
`cmd.exe` the usual `mysql -u root -p < db\setup.sql` works. You can also
open the file in MySQL Workbench or HeidiSQL and execute it.

## 4. Configure

```powershell
Copy-Item .env.example .env
notepad .env
```

Set the password in `MES_DSN` to the one you chose:

```
MES_DSN=mes:your-password@tcp(127.0.0.1:3306)/mes
```

The commands read `.env` from the folder they are run in, so there is no need
to set environment variables by hand. Leave `ANTHROPIC_API_KEY` and the
`MES_WHATSAPP_*` values empty for now: section 8 covers note drafting and
section 10 WhatsApp.

## 5. Create the schema, a practice, and run

```powershell
go run ./cmd/migrate

go run ./cmd/bootstrap -slug greenside -name "Greenside Clinic" `
  -admin-email admin@greenside.co.za -admin-name "Jane Admin"

go run ./cmd/server
```

`bootstrap` prints the administrator's password **once**. The backtick at the
end of a line is PowerShell's line continuation.

The same steps through the helper script:

```powershell
.\make.ps1 migrate
.\make.ps1 bootstrap -Slug greenside -Name "Greenside Clinic" -AdminEmail admin@greenside.co.za -AdminName "Jane Admin"
.\make.ps1 run
```

The server listens on http://localhost:8080. Check it with
`Invoke-RestMethod http://localhost:8080/healthz`.

### Upgrading from an earlier stage

Stop the server, then:

```powershell
go run ./cmd/migrate
```

This applies the migrations you do not have yet: 0002 to 0005 came with
stage 2 (medication, notes, prescriptions, lab results, documents) and 0006
with stage 3 (messages). Practices, users and patients are left as they
were. While `MES_AUTO_MIGRATE` is on, which it is by default outside
production, starting the server does the same.

### A demo practice

```powershell
.\make.ps1 seed
```

or `go run ./cmd/seed -slug demo -name "Greenside Clinic"`. In about half a
minute this builds a practice of 24 made-up patients with months of
history: notes, scripts, results, letters, and 60 days of WhatsApp
reminders and replies from simulated patients. It prints sign-ins for an
administrator, a doctor, a nurse and two patients (practice code `demo`).
Add `-password 'something-long'` to give every demo sign-in the same
password. The demo practice never sends a message: its patients and their
numbers are made up.

### Adding medicines to the catalogue

The catalogue is shared by every practice. It starts with 32 common chronic
and acute medicines. Load more from a CSV file with a header row:

```powershell
@'
generic_name,brand_name,strength,dosage_form,route,schedule,nappi_code,pack_size
Losartan,,50 mg,tablet,oral,S3,,28
Indapamide,,1.5 mg,tablet,oral,S3,,30
'@ | Set-Content -Encoding UTF8 medicines.csv

go run ./cmd/catalogue -import medicines.csv
```

It prints `catalogue import: 2 added, 0 updated, 0 rejected`. Rejected rows
are listed with the line number and the field at fault. Importing the same
product again (same generic name, brand, strength and form) updates it
instead of adding a copy. A file saved from Excel works, whether its columns
are separated by commas or by semicolons.

## 6. Try the API

In Windows PowerShell 5.1, `curl` is an alias for `Invoke-WebRequest`, not
real curl. Use `Invoke-RestMethod` as below, or `curl.exe` (built into
Windows 10 and later).

```powershell
$base = 'http://localhost:8080/api/v1'
function Api($method, $path, $body) {
    $req = @{ Method = $method; Uri = "$base$path"; Headers = $script:auth }
    if ($body) {
        # Sent as UTF-8 bytes: PowerShell 5.1 would otherwise garble names such as Zoë
        $req.ContentType = 'application/json; charset=utf-8'
        $req.Body = [Text.Encoding]::UTF8.GetBytes(($body | ConvertTo-Json -Depth 6))
    }
    try { Invoke-RestMethod @req }
    catch { $_.ErrorDetails.Message | ConvertFrom-Json | ConvertTo-Json -Depth 6 }
}

# Sign in with the password bootstrap printed
$script:auth = @{}
$login = Api POST /auth/login @{ tenant_slug = 'greenside'; email = 'admin@greenside.co.za'; password = 'PASTE-IT-HERE' }
$login.state            # 'active', or 'setup' when MES_REQUIRE_MFA=true
$script:auth = @{ Authorization = "Bearer $($login.token)" }

Api GET /me

# Change the bootstrap password
Api POST /auth/password @{ current_password = 'PASTE-IT-HERE'; new_password = 'a-long-passphrase-of-your-own' }

# Add a doctor (BR-1.5: clinical staff need a registration number)
Api POST /users @{ email = 'dr.naidoo@greenside.co.za'; full_name = 'Dr Priya Naidoo'; role = 'doctor'
                   professional_reg_no = 'MP0712345'; password = 'kettle-orbit-mango-42' }

# Register a patient; the mobile number is stored as +27821234567
$p = Api POST /patients @{ first_name = 'Thabo'; last_name = 'Nkosi'; date_of_birth = '1980-01-01'
                           sex = 'male'; national_id = '8001015009087'; mobile = '082 123 4567'
                           preferred_channel = 'whatsapp' }

# The same name and date of birth again: 409 possible_duplicate with the match
Api POST /patients @{ first_name = 'Thabo'; last_name = 'Nkosi'; date_of_birth = '1980-01-01'; sex = 'male' }

Api GET "/patients?q=Nkosi"
Api POST "/patients/$($p.id)/consents" @{ purpose = 'data_processing'; granted = $true; method = 'written' }
Api GET "/patients/$($p.id)/consents"
Api GET "/audit?limit=20"
```

### Trying two-step verification

Set `MES_REQUIRE_MFA=true` in `.env` and restart the server. Signing in then
returns `state = setup`, and everything except enrolment answers
`403 mfa_setup_required` until a factor is set up:

```powershell
$e = Api POST /auth/mfa/enrol
$e.otpauth_uri     # paste into a QR generator you trust, or type $e.secret into the app
Api POST /auth/mfa/enrol/confirm @{ code = '123456' }   # the code your app shows
```

The confirm call returns ten recovery codes, shown only this once. From then
on, signing in returns `state = pending` and a short-lived token that can do
nothing except:

```powershell
$script:auth = @{ Authorization = "Bearer $($login.token)" }
$full = Api POST /auth/mfa/verify @{ code = '654321' }   # or @{ recovery_code = 'XXXX-XXXX-XXXX-XXXX' }
$script:auth = @{ Authorization = "Bearer $($full.token)" }
```

A code works once. Signing in within the same 30 seconds as enrolling needs
the next code.

## 7. Clinical records

These examples carry on from section 6, in the same PowerShell window. Only
a doctor can start medication, prescribe or issue a document, so sign in as
the doctor added above. A nurse can write notes, record results and prepare
documents. The practice administrator sees none of this.

```powershell
$script:auth = @{}
$login = Api POST /auth/login @{ tenant_slug = 'greenside'; email = 'dr.naidoo@greenside.co.za'; password = 'kettle-orbit-mango-42' }
$script:auth = @{ Authorization = "Bearer $($login.token)" }
$patient = "/patients/$($p.id)"    # the patient registered in section 6

# Repeat prescriptions are for patients on the chronic register
Api POST "$patient/conditions" @{ icd10_code = 'E11.9'; description = 'Type 2 diabetes mellitus'; is_chronic = $true }
Api POST "$patient/allergies" @{ substance = 'Penicillin'; reaction = 'Rash'; severity = 'moderate' }
```

(With `MES_REQUIRE_MFA=true` the doctor enrols first, as in section 6.)

**Medication.** A regimen records what the patient has been told to take,
using a medicine from the catalogue:

```powershell
$med = (Api GET "/medications?q=metformin").items | Where-Object strength -eq '500 mg'
$reg = Api POST "$patient/regimens" @{ medication_id = $med.id; dose = '1 tablet'; frequency = 'twice_daily'
                                       instructions = 'With breakfast and supper' }
Api GET "$patient/regimens"
```

**Consultation notes.** A note is a draft until its author signs it. After
that it cannot be edited, only amended, and every amendment keeps the
original wording:

```powershell
$note = Api POST "$patient/encounters" @{ encounter_type = 'follow_up'
    presenting_complaint = 'Diabetes review'; examination = 'BP 142/88. Feet: sensation intact.'
    assessment = 'Type 2 diabetes, suboptimal control'; plan = 'Continue metformin. HbA1c in 3 months.'
    vitals = @{ systolic_bp = 142; diastolic_bp = 88; weight_kg = 91.5 } }
Api POST "/encounters/$($note.id)/sign"

Api POST "/encounters/$($note.id)/amendments" @{ field = 'plan'
    new_value = 'Continue metformin. HbA1c and potassium in 3 months.'; reason = 'Potassium test added after signing' }
(Api GET "/encounters/$($note.id)").amendments
```

`PATCH /encounters/{id}` saves a draft. Send the whole note: the text fields
and vitals are replaced by what you send. The type and time are kept if you
leave them out.

**Prescriptions.** Items are copied from the patient's regimens. There are
at most 5 repeats, and a script is valid for at most 6 months. A patient
without a signed note in the last 6 months (`MES_DEFAULT_REVIEW_MONTHS`)
needs an `override_reason`.

```powershell
$rx = Api POST "$patient/prescriptions" @{ repeats_authorised = 5
          items = @(@{ regimen_id = $reg.id; quantity = '60 tablets' }) }
$rx.notice                        # no interaction or allergy checks were performed
$rx.recorded_allergies            # listed for your information, not checked
$rx.prescription.valid_until

$next = Api POST "/prescriptions/$($rx.prescription.id)/repeat"
(Api GET "/prescriptions/$($rx.prescription.id)").repeats_used    # 1
```

**Laboratory results.** Captured as the laboratory printed them, flags
included. The inbox lists unacknowledged results, critical ones first:

```powershell
$lab = Api POST "$patient/lab-results" @{ lab_name = 'City Pathology'; lab_reference = 'CP-889911'
    panel = 'HbA1c, urea and electrolytes'
    items = @(
        @{ analyte = 'HbA1c'; value = '9.1'; unit = '%'; reference_range = '< 7.0'; abnormal_flag = 'high' }
        @{ analyte = 'Potassium'; value = '6.4'; unit = 'mmol/L'; reference_range = '3.5 - 5.1'; abnormal_flag = 'critical_high' }
    ) }

(Api GET "/lab-results").items | Select-Object panel, has_critical, has_abnormal, reported_at
Api POST "/lab-results/$($lab.id)/acknowledge" @{ note = 'Phoned the patient; repeat potassium today' }
```

**Sick notes and referral letters.** Both start as drafts. Issuing numbers
the document (`MC-2026-0001`, `REF-2026-0001`, per practice and year) and
freezes its text:

```powershell
$today = Get-Date -Format 'yyyy-MM-dd'
$cert = Api POST "$patient/sick-notes" @{ basis = 'examination'; examined_on = $today; valid_from = $today
                                          valid_to = (Get-Date).AddDays(2).ToString('yyyy-MM-dd') }
$cert = Api POST "/documents/$($cert.id)/issue"
$cert.document_number
$cert.rendered_text

$ref = Api POST "$patient/referrals" @{ to_name = 'Dr S. Pillay'; to_speciality = 'Nephrology'; urgency = 'urgent'
    reason = 'Hyperkalaemia on ACE inhibitor'; clinical_summary = 'K 6.4 mmol/L today, HbA1c 9.1%.'
    include_medication = $true; include_allergies = $true; encounter_id = $note.id }
(Api POST "/documents/$($ref.id)/issue").rendered_text
```

A sick note leaves the diagnosis off unless you set `disclose_diagnosis` with
the patient's agreement. A certificate that starts before the consultation
needs a `backdate_reason`, and it says that it is backdated.

The patient's summary brings it together:

```powershell
Api GET "$patient/summary" | ConvertTo-Json -Depth 6
```

## 8. AI note drafting

Drafting turns a clinician's dictation into the five sections of a note. It
is off until an Anthropic API key is set. Only set one once a data
processing agreement is in place, because dictations are patient data. The
patient's name and ID number are replaced with placeholders before anything
is sent.

1. Add the key to `.env` and restart the server:

   ```
   ANTHROPIC_API_KEY=sk-ant-...
   ```

   The server logs `note drafting enabled` at start-up. Without a key it logs
   `note drafting disabled`, and drafting requests answer `503 ai_unavailable`.

2. Record the patient's consent. Without it, nothing is sent and the request
   answers `409 ai_consent_required`:

   ```powershell
   Api POST "$patient/consents" @{ purpose = 'ai_assisted_review'; granted = $true; method = 'verbal' }
   ```

3. Draft into an unsigned note, read it, and copy what you accept into the
   note yourself:

   ```powershell
   $visit = Api POST "$patient/encounters" @{ presenting_complaint = 'Cough' }
   $d = Api POST "/encounters/$($visit.id)/draft-note" @{
       dictation = 'Three days of dry cough, no fever, non-smoker. Chest clear. Viral URTI. Fluids and rest, come back if worse.' }
   $d.notice
   $d.draft.draft          # presenting_complaint, history, examination, assessment, plan
   $d.draft.draft.flags    # things the draft could not place or found unclear

   $x = $d.draft.draft
   Api PATCH "/encounters/$($visit.id)" @{ presenting_complaint = $x.presenting_complaint; history = $x.history
       examination = $x.examination; assessment = $x.assessment; plan = $x.plan }
   Api POST "/encounters/$($visit.id)/sign"
   ```

The draft is stored beside the note, never in it, and the note records that
drafting was used. If a draft is wrong in a way worth reviewing, flag it:
`Api POST "/encounters/$($visit.id)/drafts/$($d.draft.id)/flag" @{ reason = '...' }`.
Each practice can make `MES_AI_DAILY_LIMIT` drafting requests a day (200 by
default). After that, requests answer `429 ai_limit_reached` until midnight.

## 9. Practice settings, dashboard and recall lists

Practice details and settings belong to the administrator. The dashboard
and recall lists are for any staff member.

```powershell
$doctorAuth = $script:auth           # the doctor from section 7
$script:auth = @{}
$admin = Api POST /auth/login @{ tenant_slug = 'greenside'; email = 'admin@greenside.co.za'; password = 'a-long-passphrase-of-your-own' }
$adminAuth = @{ Authorization = "Bearer $($admin.token)" }
$script:auth = $adminAuth

Api PATCH /practice @{ phone = '011 555 0123'; address = '12 Main Road, Greenside'; practice_number = '0123456' }
(Api GET /practice).practice

# Counts only: nothing on the dashboard identifies a patient
$dash = Api GET '/dashboard?days=30'
$dash.patients
$dash.adherence
$dash.notes

# Patients to phone
(Api GET '/recalls?reason=review_overdue').items | Select-Object first_name, last_name, mobile_display, last_review
(Api GET '/recalls?reason=script_expiring&days=14').items | Select-Object first_name, last_name, valid_until, repeats_left
(Api GET '/recalls?reason=not_responding').items | Select-Object first_name, last_name, doses_due, doses_answered

# Unreviewed abnormal results are a list for clinicians only
$script:auth = $doctorAuth
(Api GET '/recalls?reason=unacknowledged_result').items | Select-Object first_name, last_name, panel, critical
```

With only the examples above, the lists are short or empty: the patient in
section 7 was seen today. The demo practice (section 5) has plenty on every
list; sign in to it to see them filled.

## 10. WhatsApp reminders

Reminders need a WhatsApp Business account on the server and approved
message templates: [WHATSAPP.md](WHATSAPP.md) covers both. Without them, the
settings below are saved and the last calls answer
`503 messaging_not_configured`.

**From here on, messages are real.** Use your own mobile number for the test
patient: the example number in section 6 belongs to somebody.

```powershell
$script:auth = $adminAuth
Api PATCH /practice @{ whatsapp_enabled = $true }
(Api GET /practice).messaging              # active is True with a WhatsApp account on the server

$t = Api POST /patients @{ first_name = 'Test'; last_name = 'Patient'; date_of_birth = '1990-01-01'; sex = 'female'
                           mobile = 'YOUR-OWN-NUMBER'; preferred_channel = 'whatsapp' }
Api POST "/patients/$($t.id)/consents" @{ purpose = 'reminder_communications'; granted = $true; method = 'verbal' }

# A recall straight away; the preview is the message as it arrives
(Api POST "/patients/$($t.id)/notifications/recall").preview
Api GET /notifications/health

# A medicine, so reminders are scheduled; the first arrives at the next dose time
$script:auth = $doctorAuth
Api POST "/patients/$($t.id)/conditions" @{ icd10_code = 'I10'; description = 'Essential hypertension'; is_chronic = $true }
$aml = (Api GET '/medications?q=amlodipine').items | Where-Object strength -eq '5 mg'
Api POST "/patients/$($t.id)/regimens" @{ medication_id = $aml.id; dose = '1 tablet'; frequency = 'once_daily' }
(Api GET "/patients/$($t.id)/doses").items | Select-Object scheduled_at, status, source
(Api GET "/patients/$($t.id)/notifications").notifications | Select-Object purpose, status, preview
```

When the reminder arrives, press **Taken**. Once the webhook is set up
(WHATSAPP.md, section 3), the dose shows `taken` with `response_channel`
`whatsapp`, and your reply appears under `replies` in the patient's
notifications. Reply STOP to withdraw the consent again.

## 11. Tests

```powershell
.\make.ps1 test                      # unit tests, no database needed

$env:MES_TEST_DSN = 'mes:your-password@tcp(127.0.0.1:3306)/mes_test'
.\make.ps1 test-integration          # everything, against the mes_test database
```

The integration tests create their own practices with random codes, so the
test database never needs resetting. They never call Anthropic or Meta:
drafting and WhatsApp are tested against stand-ins.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `make.ps1 cannot be loaded because running scripts is disabled` | Run `Get-ChildItem -Recurse \| Unblock-File`, or `powershell -ExecutionPolicy Bypass -File .\make.ps1 test` |
| `Access denied for user 'mes'@'localhost'` | The password in `.env` does not match `db\setup.sql`, or the script was not run |
| `cannot reach the database at 127.0.0.1:3306` | The MySQL/MariaDB service is not running: `Get-Service *mysql*, *maria*` |
| `1 migration(s) pending` | Run `go run ./cmd/migrate` |
| `bind: Only one usage of each socket address` | Port 8080 is taken; set `MES_ADDR=:8081` in `.env` |
| Sign-in always fails | The practice code is the `-slug` you gave bootstrap, not the practice name. Five wrong passwords lock the account for 15 minutes |
| `go: ... dial tcp: lookup proxy.golang.org` | The first build downloads one dependency; it needs internet access once |
| `403 doctor_only` | Starting medication, prescribing and issuing documents need a doctor's account |
| `409 not_chronic` | The patient has no active chronic condition; add one with `is_chronic = $true` |
| `409 review_overdue` | No signed note within the review interval. Sign one, or send `override_reason` |
| `503 ai_unavailable` | `ANTHROPIC_API_KEY` is empty, or the server was not restarted after setting it |
| `the header row has no generic_name column` | The CSV's first line is not the header, or it was saved as UTF-16. PowerShell 5.1's `>` and `Out-File` do that; use `Set-Content -Encoding UTF8`, or save as "CSV UTF-8" in Excel |
| Accented names (Zoë) are stored garbled | The request body was not sent as UTF-8. Use the `Api` function above, which sends UTF-8 bytes |
| `503 messaging_not_configured` | The server has no WhatsApp account: set the `MES_WHATSAPP_*` values (WHATSAPP.md) and restart |
| `409 messaging_off` | The practice has not switched messaging on: `PATCH /practice` with `whatsapp_enabled = $true` |
| `422 clinical_detail` when switching messaging on | The name patients would see says what the practice treats, or names a medicine; set a neutral `messaging_name` |
| `409 comms_consent_required` | Record the patient's `reminder_communications` consent first |
| `409 no_whatsapp` | The patient has no mobile number, or prefers another channel; the message says which |
| A patient on WhatsApp has no doses scheduled | Doses are only scheduled for patients who can answer: messaging on for the practice, their consent, and a number. The hourly run catches up within the hour |
| Replies never arrive | The webhook is not set up, or `MES_WHATSAPP_APP_SECRET` is missing or wrong: the server logs `whatsapp webhook refused` |
