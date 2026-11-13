$ErrorActionPreference = 'Stop'
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../.installer-source/public/install.ps1'), [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$definitions = $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)
foreach ($name in @('Test-ShouldSkipStep', 'Step-Llm', 'Read-Prompt', 'Step-Admin', 'Step-Ports', 'Step-Domains', 'Step-Github', 'Step-Skills', 'Start-AgentTeamsDashboard')) {
    Invoke-Expression ($definitions | Where-Object Name -eq $name | Select-Object -First 1).Extent.Text
}
function Write-Log { param($Message) }
function Get-Msg { param($Key) return $Key }
function Read-Host { throw 'Fresh install must not prompt' }
function New-RandomKey { return 'synthetic-random-password' }
$script:AGENTTEAMS_QUICKSTART = $true
$script:AGENTTEAMS_NON_INTERACTIVE = $false
$script:AGENTTEAMS_LANGUAGE = 'zh'
$script:config = @{}
foreach ($key in @('AGENTTEAMS_LLM_API_KEY', 'AGENTTEAMS_EMBEDDING_MODEL', 'AGENTTEAMS_ADMIN_USER', 'AGENTTEAMS_ADMIN_PASSWORD', 'AGENTTEAMS_DASHBOARD', 'AGENTTEAMS_DASHBOARD_IMAGE', 'AGENTTEAMS_PORT_DASHBOARD')) {
    [Environment]::SetEnvironmentVariable($key, $null, 'Process')
}
foreach ($step in @('Step-Lang', 'Step-Mode', 'Step-Llm', 'Step-ManagerRuntime', 'Step-Runtime', 'Step-Admin', 'Step-Network', 'Step-Ports', 'Step-Domains', 'Step-Github', 'Step-Skills', 'Step-Volume', 'Step-Workspace', 'Step-E2ee', 'Step-DockerProxy', 'Step-Idle', 'Step-Hostshare')) {
    if (-not (Test-ShouldSkipStep $step)) { & $step }
}
if ($script:config.LLM_API_KEY -or $script:config.EMBEDDING_MODEL) { throw 'API key and embedding should remain empty' }
if ($script:config.ADMIN_USER -ne 'admin' -or -not $script:config.ADMIN_PASSWORD) { throw 'Admin credentials missing' }
$env:AGENTTEAMS_LLM_API_KEY = 'synthetic-key'
$env:AGENTTEAMS_DEFAULT_MODEL = 'custom-model'
$env:AGENTTEAMS_OPENAI_BASE_URL = 'https://example.invalid/v1'
Step-Llm
if ($script:config.LLM_API_KEY -ne 'synthetic-key' -or $script:config.DEFAULT_MODEL -ne 'custom-model' -or $script:config.OPENAI_BASE_URL -ne 'https://example.invalid/v1') { throw 'Environment model was overwritten' }

# Mock the complete Dashboard launch to inspect startup and desktop handoff.
$script:AGENTTEAMS_USE_EMBEDDED = '1'
$script:AGENTTEAMS_REGISTRY = 'example.invalid'
$env:AGENTTEAMS_AUTH_TOKEN = 'synthetic-admin-token'
$script:config.MINIO_USER = 'admin'
$script:config.MINIO_PASSWORD = 'synthetic-storage-password'
$script:dockerCalls = [System.Collections.Generic.List[string]]::new()
function docker { $script:dockerCalls.Add(($args -join ' ')); $global:LASTEXITCODE = 0 }
function Invoke-WebRequest { return @{ StatusCode = 200 } }
function Set-Clipboard { param($Value) $script:copiedPassword = $Value }
function Start-Process { param($FilePath) $script:openedUrl = $FilePath }
Start-AgentTeamsDashboard
if ($script:copiedPassword -ne $script:config.ADMIN_PASSWORD) { throw 'Password was not copied' }
if ($script:openedUrl -ne 'http://127.0.0.1:13000/?username=admin') { throw 'Wrong Dashboard URL' }
if ($script:openedUrl.Contains($script:config.ADMIN_PASSWORD)) { throw 'Password leaked to URL' }
if (-not ($script:dockerCalls | Where-Object { $_ -match 'run -d --name agentteams-dashboard' })) { throw 'Dashboard was not started' }
Write-Host 'PowerShell one-click install tests passed'
