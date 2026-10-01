# Register this machine as a Forge CI runner. Windows.
#
# Prereq: Git for Windows (https://git-scm.com/download/win) — the shell
# executor runs each job's script through `sh`, which Git for Windows ships
# and puts on PATH. Most Windows dev machines already have it (git itself is
# needed for job checkouts anyway).
#
# Usage (the Runner tokens page gives you this exact line, filled in):
#   $env:SERVER="<url>"; $env:TOKEN="<token>"; $env:TAGS="<tags>"; iwr <server>/api/v1/downloads/install-runner.ps1 -UseBasicParsing | iex
#
# Env vars: SERVER (required), TOKEN, TAGS (default qa-laptop), ID, DIR (default .)

$ErrorActionPreference = "Stop"

if (-not $env:SERVER) {
    throw "SERVER is required, e.g. `$env:SERVER='https://forge-ci.example.com'"
}
$Tags = if ($env:TAGS) { $env:TAGS } else { "qa-laptop" }
$Id   = if ($env:ID)   { $env:ID }   else { "$env:COMPUTERNAME-$([int][double]::Parse((Get-Date -UFormat %s)))" }
$Dir  = if ($env:DIR)  { $env:DIR }  else { "." }

if (-not (Get-Command sh -ErrorAction SilentlyContinue)) {
    Write-Warning "sh.exe not found on PATH — install Git for Windows first: https://git-scm.com/download/win"
}

$BinPath = Join-Path $Dir "forge-runner.exe"
Write-Host "==> downloading forge-runner-windows-amd64.exe"
Invoke-WebRequest -Uri "$($env:SERVER)/api/v1/downloads/forge-runner-windows-amd64.exe" -OutFile $BinPath -UseBasicParsing

Write-Host "==> registered as:  id=$Id  tags=$Tags  server=$($env:SERVER)"
Write-Host ""
if ($env:TOKEN) {
    Write-Host "Starting the runner (Ctrl-C to stop):"
    Write-Host ""
    & $BinPath --server=$env:SERVER --id=$Id --executor=shell --tags=$Tags --token=$env:TOKEN
} else {
    Write-Host "Run this to start it:"
    Write-Host ""
    Write-Host "  $BinPath --server=$($env:SERVER) --id=$Id --executor=shell --tags=$Tags"
}
