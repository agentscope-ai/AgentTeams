$ErrorActionPreference = 'Stop'
$source = Get-Content (Join-Path $PSScriptRoot '../install/agentteams-install.ps1') -Raw
$start = $source.IndexOf('    $script:MANAGER_IMAGE = if')
$end = $source.IndexOf('    # Resolve embedded controller image', $start)
if ($start -lt 0 -or $end -lt 0) { throw 'Image resolution block missing' }
$resolver = $source.Substring($start, $end - $start)
function Test-DeepSeekHarnessAvailable { return $true }
Get-ChildItem Env:AGENTTEAMS_* | Remove-Item
$script:AGENTTEAMS_REGISTRY = 'registry.example'
$script:AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION = 'v1.2.7'
$script:AGENTTEAMS_CODEX_WORKER_VERSION = 'v1.2.7'
$script:AGENTTEAMS_VERSION = 'v1.2.7'
Invoke-Expression $resolver
foreach ($name in 'WORKER_IMAGE','QWENPAW_WORKER_IMAGE','HERMES_WORKER_IMAGE','OPENHUMAN_WORKER_IMAGE','DEEPSEEK_HARNESS_WORKER_IMAGE','CODEX_WORKER_IMAGE') {
    if ((Get-Variable $name -Scope Script -ValueOnly) -notlike 'registry.example/agentteams/agentteams-harness-*:v1.2.7') { throw "Incorrect unified version: $name" }
}
$script:AGENTTEAMS_VERSION = 'v1.2.6'
Invoke-Expression $resolver
if ($script:DEEPSEEK_HARNESS_WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-deepseek-harness-worker:v0.1.1') { throw 'Historical DeepSeek fallback missing' }
if ($script:CODEX_WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-harness-codex:v0.1.0') { throw 'Historical Codex fallback missing' }
$env:AGENTTEAMS_CODEX_WORKER_VERSION = 'custom'
$script:AGENTTEAMS_CODEX_WORKER_VERSION = 'custom'
Invoke-Expression $resolver
if ($script:CODEX_WORKER_IMAGE -notlike '*:custom') { throw 'Explicit Codex version ignored' }
Write-Host 'PASS: PowerShell unified Harness versions, historical fallback and explicit override'
