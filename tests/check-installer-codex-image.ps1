$ErrorActionPreference = 'Stop'
$tokens = $null
$parseErrors = $null
$installer = Join-Path $PSScriptRoot '../install/agentteams-install.ps1'
$ast = [System.Management.Automation.Language.Parser]::ParseFile(
    $installer, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$assignment = $ast.Find({ param($node)
    $node -is [System.Management.Automation.Language.AssignmentStatementAst] -and
    $node.Left.Extent.Text -eq '$script:CODEX_WORKER_IMAGE'
}, $true)
if (-not $assignment) { throw 'Codex image resolver missing' }
$script:AGENTTEAMS_CODEX_WORKER_VERSION = 'v0.1.0'
$script:AGENTTEAMS_REGISTRY = 'example.test'
Remove-Item Env:AGENTTEAMS_INSTALL_CODEX_WORKER_IMAGE -ErrorAction SilentlyContinue
Remove-Item Env:AGENTTEAMS_CODEX_WORKER_IMAGE -ErrorAction SilentlyContinue
Invoke-Expression $assignment.Extent.Text
if ($script:CODEX_WORKER_IMAGE -ne 'example.test/agentteams/agentteams-harness-codex:v0.1.0') {
    throw 'Default Codex image missing'
}
$env:AGENTTEAMS_CODEX_WORKER_IMAGE = 'example.test/codex:saved'
Invoke-Expression $assignment.Extent.Text
if ($script:CODEX_WORKER_IMAGE -ne $env:AGENTTEAMS_CODEX_WORKER_IMAGE) {
    throw 'Saved Codex image ignored'
}
$env:AGENTTEAMS_INSTALL_CODEX_WORKER_IMAGE = 'example.test/codex:override'
Invoke-Expression $assignment.Extent.Text
if ($script:CODEX_WORKER_IMAGE -ne $env:AGENTTEAMS_INSTALL_CODEX_WORKER_IMAGE) {
    throw 'Codex image override ignored'
}
Write-Host 'PASS: PowerShell Codex default image and overrides'
