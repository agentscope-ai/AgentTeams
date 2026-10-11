$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot '../install/agentteams-install.ps1'
$source = Get-Content $installer -Raw
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($installer, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$step = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Step-Admin' }, $true)
Invoke-Expression $step.Extent.Text
$minioBlock = [regex]::Match($source, '(?ms)^    \$config.MINIO_USER =.*?^    \$config.MINIO_PASSWORD =[^\r\n]*').Value
function Get-Msg { param($Key) $Key }
function Write-Log {}
function Write-Error { param($Message) throw $Message }
function Read-Prompt {
    param($VarName, $PromptText, $Default)
    $script:calls++
    if ($script:calls -gt 2) { throw 'Repeated invalid preset' }
    if ($script:back) { $script:StepResult = 'back'; return $null }
    if ($script:calls -eq 1 -and $script:firstInput) { return $script:firstInput }
    $value = [Environment]::GetEnvironmentVariable($VarName)
    if ($value) { return $value }
    if ($script:answer) { return $script:answer }
    return $Default
}
function Test-Admin($UserName, $NonInteractive, $Answer, $Back = $false, $FirstInput = '') {
    $script:config = @{}
    $script:AGENTTEAMS_NON_INTERACTIVE = $NonInteractive
    $script:answer = $Answer
    $script:back = $Back
    $script:StepResult = ''
    $script:firstInput = $FirstInput
    $script:calls = 0
    $env:AGENTTEAMS_ADMIN_USER = $UserName
    $env:AGENTTEAMS_ADMIN_PASSWORD = 'password'
    Step-Admin
    if ($Back) {
        if ($script:StepResult -ne 'back') { throw 'Back navigation failed' }
    } elseif ($script:config.ADMIN_USER -cne $(if ($Answer) { $Answer } else { 'admin' })) {
        throw 'Unexpected username'
    }
}
$rejected = $false
try { Test-Admin 'ss' $true 'ss' } catch {
    if ($_.Exception.Message -ne 'admin.username_too_short') { throw }
    $rejected = $true
}
if (-not $rejected) { throw 'Two-character preset accepted' }
Test-Admin 'ABC' $true 'abc'
Test-Admin '' $true ''
Test-Admin 'ss' $false 'valid'
Test-Admin '' $false 'valid' $false 'ss'
Test-Admin '' $false '' $true
foreach ($userName in @('ss', 'abc', '')) {
    $config = @{ ADMIN_USER = 'abc'; ADMIN_PASSWORD = 'password' }
    $env:AGENTTEAMS_MINIO_USER = $userName
    $rejected = $false
    try { Invoke-Expression $minioBlock } catch { $rejected = $true }
    if ($rejected -ne ($userName -eq 'ss')) { throw "Wrong MinIO validation result: $userName" }
    if (-not $rejected -and $config.MINIO_USER -ne 'abc') { throw 'Unexpected MinIO username' }
}
Write-Host 'PASS: PowerShell installer username validation'
