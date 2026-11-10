$ErrorActionPreference = 'Stop'
$source = Join-Path $PSScriptRoot '../install/agentteams-install.ps1'
$text = Get-Content -LiteralPath $source -Raw
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($source, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw $errors[0] }
$function = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Test-DeepSeekHarnessAvailable' }, $true)
Invoke-Expression $function.Extent.Text
$start = $text.IndexOf('    # Set image names')
$end = $text.IndexOf('    # Resolve embedded controller image', $start)
if ($start -lt 0 -or $end -lt 0) { throw 'Image resolution block missing' }
$resolve = [scriptblock]::Create($text.Substring($start, $end - $start))
$script:AGENTTEAMS_REGISTRY = 'registry.example'
$script:AGENTTEAMS_KNOWN_STABLE_VERSION = 'v1.2.7'
$script:AGENTTEAMS_DEEPSEEK_HARNESS_MIN_VERSION = 'v1.2.4'
$script:AGENTTEAMS_CODEX_WORKER_VERSION = 'v0.1.0'
$names = @('WORKER', 'QWENPAW_WORKER', 'HERMES_WORKER', 'OPENHUMAN_WORKER', 'DEEPSEEK_HARNESS_WORKER')
$saved = @{}
foreach ($name in $names) {
    $key = "AGENTTEAMS_INSTALL_${name}_IMAGE"
    $saved[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
    [Environment]::SetEnvironmentVariable($key, $null, 'Process')
}
try {
    $script:AGENTTEAMS_VERSION = 'v1.2.6'
    $script:AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION = 'v0.1.1'
    & $resolve
    if ($script:WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-worker:v1.2.6') { throw 'Historical OpenClaw image changed' }
    if ($script:QWENPAW_WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-qwenpaw-worker:v1.2.6') { throw 'Historical QwenPaw image changed' }
    if ($script:DEEPSEEK_HARNESS_WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-deepseek-harness-worker:v0.1.1') { throw 'Historical DeepSeek image changed' }
    $script:AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION = 'v0.1.2'
    & $resolve
    if (-not $env:AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION -and $script:DEEPSEEK_HARNESS_WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-deepseek-harness-worker:v0.1.1') { throw 'Historical default DeepSeek release changed' }
    $script:AGENTTEAMS_VERSION = 'v1.2.7'
    $script:AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION = 'v0.1.2'
    & $resolve
    foreach ($entry in @(@('WORKER','openclaw'), @('QWENPAW_WORKER','qwenpaw'), @('HERMES_WORKER','hermes'), @('OPENHUMAN_WORKER','openhuman'))) {
        if ((Get-Variable -Scope Script -Name "$($entry[0])_IMAGE" -ValueOnly) -ne "registry.example/agentteams/agentteams-harness-$($entry[1]):v1.2.7") { throw "Harness name mismatch: $($entry[1])" }
    }
    if ($script:DEEPSEEK_HARNESS_WORKER_IMAGE -ne 'registry.example/agentteams/agentteams-harness-deepseek:v0.1.2') { throw 'DeepSeek Harness name mismatch' }
    foreach ($name in $names) { [Environment]::SetEnvironmentVariable("AGENTTEAMS_INSTALL_${name}_IMAGE", "private.example/${name}:custom", 'Process') }
    $script:AGENTTEAMS_VERSION = 'v1.2.6'
    & $resolve
    foreach ($name in $names) {
        if ((Get-Variable -Scope Script -Name "${name}_IMAGE" -ValueOnly) -ne "private.example/${name}:custom") { throw "Custom image changed: $name" }
    }
    Write-Output 'PASS: PowerShell Harness names, historical releases and explicit image overrides'
} finally {
    foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key, $saved[$key], 'Process') }
}
