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
to set environment variables by hand.

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

## 6. Try the API

In Windows PowerShell 5.1, `curl` is an alias for `Invoke-WebRequest`, not
real curl. Use `Invoke-RestMethod` as below, or `curl.exe` (built into
Windows 10 and later).

```powershell
$base = 'http://localhost:8080/api/v1'
function Api($method, $path, $body) {
    $req = @{ Method = $method; Uri = "$base$path"; Headers = $script:auth }
    if ($body) { $req.ContentType = 'application/json'; $req.Body = ($body | ConvertTo-Json -Depth 5) }
    try { Invoke-RestMethod @req }
    catch { $_.ErrorDetails.Message | ConvertFrom-Json | ConvertTo-Json -Depth 5 }
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

## 7. Tests

```powershell
.\make.ps1 test                      # unit tests, no database needed

$env:MES_TEST_DSN = 'mes:your-password@tcp(127.0.0.1:3306)/mes_test'
.\make.ps1 test-integration          # everything, against the mes_test database
```

The integration tests create their own practices with random codes, so the
test database never needs resetting.

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
