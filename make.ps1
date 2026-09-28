<#
.SYNOPSIS
  Build, test and run tasks for Windows - the equivalent of the Makefile.

.EXAMPLE
  .\make.ps1 test
  .\make.ps1 migrate
  .\make.ps1 bootstrap -Slug greenside -Name "Greenside Clinic" -AdminEmail admin@greenside.co.za -AdminName "Jane Admin"
  .\make.ps1 run

.NOTES
  If Windows refuses to run this script, the files were probably marked as
  downloaded. From this folder run:
    Get-ChildItem -Recurse | Unblock-File
  or start it with:
    powershell -ExecutionPolicy Bypass -File .\make.ps1 test
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('help', 'build', 'test', 'test-integration', 'vet', 'migrate', 'check', 'bootstrap', 'run')]
    [string]$Task = 'help',

    [string]$Slug,
    [string]$Name,
    [string]$AdminEmail,
    [string]$AdminName
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

function Invoke-Go {
    & go @args
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Error 'Go is not on PATH. Install Go 1.22 or later from https://go.dev/dl/ and open a new terminal.'
}

switch ($Task) {
    'build' {
        Invoke-Go build -o bin/ ./cmd/...
        Write-Host 'Built bin\server.exe, bin\migrate.exe and bin\bootstrap.exe'
    }
    'test' {
        Invoke-Go test ./...
    }
    'test-integration' {
        if (-not $env:MES_TEST_DSN) {
            Write-Error "Set MES_TEST_DSN first, for example:`n  `$env:MES_TEST_DSN = 'mes:change-this-password@tcp(127.0.0.1:3306)/mes_test'"
        }
        Invoke-Go test -tags=integration -count=1 ./...
    }
    'vet' {
        Invoke-Go vet -tags=integration ./...
    }
    'migrate' {
        Invoke-Go run ./cmd/migrate
    }
    'check' {
        & go run ./cmd/migrate -check
        exit $LASTEXITCODE
    }
    'bootstrap' {
        if (-not ($Slug -and $Name -and $AdminEmail -and $AdminName)) {
            Write-Error 'bootstrap needs -Slug, -Name, -AdminEmail and -AdminName'
        }
        Invoke-Go run ./cmd/bootstrap -slug $Slug -name $Name -admin-email $AdminEmail -admin-name $AdminName
    }
    'run' {
        Invoke-Go run ./cmd/server
    }
    default {
        Write-Host @'
.\make.ps1 build              build bin\server.exe, bin\migrate.exe, bin\bootstrap.exe
.\make.ps1 test               unit tests (no database needed)
.\make.ps1 test-integration   all tests; needs $env:MES_TEST_DSN
.\make.ps1 vet                go vet, including integration tests
.\make.ps1 migrate            apply pending migrations
.\make.ps1 check              exit 2 if migrations are pending
.\make.ps1 bootstrap -Slug ... -Name ... -AdminEmail ... -AdminName ...
.\make.ps1 run                run the API server
'@
    }
}
