#!/usr/bin/env powershell
# Deprecated compatibility entrypoint. Use agentteams-import.ps1.
& (Join-Path $PSScriptRoot "agentteams-import.ps1") @args
exit $LASTEXITCODE
