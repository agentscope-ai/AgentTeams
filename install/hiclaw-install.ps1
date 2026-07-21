#!/usr/bin/env powershell
# Deprecated compatibility entrypoint. Use agentteams-install.ps1.

Write-Warning "install/hiclaw-install.ps1 is deprecated; use install/agentteams-install.ps1."

$currentInstaller = Join-Path $PSScriptRoot "agentteams-install.ps1"
if (Test-Path $currentInstaller) {
    & $currentInstaller @args
    exit $LASTEXITCODE
}

$ref = if ($env:AGENTTEAMS_VERSION -and $env:AGENTTEAMS_VERSION -ne "latest") { $env:AGENTTEAMS_VERSION } else { "main" }
$tempInstaller = Join-Path ([System.IO.Path]::GetTempPath()) "agentteams-install-$([guid]::NewGuid()).ps1"
try {
    Invoke-WebRequest -UseBasicParsing -Uri "https://raw.githubusercontent.com/agentscope-ai/AgentTeams/$ref/install/agentteams-install.ps1" -OutFile $tempInstaller
    & $tempInstaller @args
    exit $LASTEXITCODE
} finally {
    Remove-Item -Force -ErrorAction SilentlyContinue $tempInstaller
}
