$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot '../.installer-source/public/install.ps1'
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($installer, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
foreach ($name in @('New-EnvFile', 'Load-CurrentParamsFromEnv')) {
    $fn = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name }, $true)
    Invoke-Expression $fn.Extent.Text
}
$resolve = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$config.HIGRESS_WASM_PLUGIN_REGISTRY' }, $true)
$argsNode = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$ctrlArgs' }, $true)
function Write-Log {}
function Get-Msg { param($Key) $Key }
$tempDir = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
New-Item -ItemType Directory $tempDir | Out-Null
$saved = $env:AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY
try {
    foreach ($case in @(
        @('higress-registry.us-west-1.cr.aliyuncs.com', '', 'higress-registry.cn-hangzhou.cr.aliyuncs.com'),
        @('higress-registry.ap-southeast-7.cr.aliyuncs.com', '', 'higress-registry.cn-hangzhou.cr.aliyuncs.com'),
        @('images.example.com', 'plugins.example.com', 'plugins.example.com')
    )) {
        $script:config = @{ REGISTRY = $case[0] }
        $config = $script:config
        $env:AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY = $case[1]
        if ($resolve) { Invoke-Expression $resolve.Extent.Text }
        $script:AGENTTEAMS_ENV_FILE = Join-Path $tempDir 'manager.env'
        New-EnvFile -Config $config -Path $script:AGENTTEAMS_ENV_FILE
        $lines = Get-Content $script:AGENTTEAMS_ENV_FILE
        if ($lines -cnotcontains "HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=$($case[2])") { throw 'Wrong saved WASM registry' }
        $script:WORKER_IMAGE = "$($case[0])/agentteams/agentteams-harness-openclaw:test"
        Invoke-Expression $argsNode.Extent.Text
        if ($ctrlArgs -cnotcontains "HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=$($case[2])") { throw 'WASM registry not forwarded to embedded Controller' }
        if ($ctrlArgs -cnotcontains "AGENTTEAMS_WORKER_IMAGE=$script:WORKER_IMAGE") { throw 'Worker image registry changed' }
    }
    $env:AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY = ''
    $script:config = @{}
    Load-CurrentParamsFromEnv
    $config = $script:config
    Invoke-Expression $resolve.Extent.Text
    if ($config.HIGRESS_WASM_PLUGIN_REGISTRY -ne 'plugins.example.com') { throw 'Saved override lost on upgrade' }
    $env:AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY = 'override.example.com'
    Invoke-Expression $resolve.Extent.Text
    if ($config.HIGRESS_WASM_PLUGIN_REGISTRY -ne 'override.example.com') { throw 'Explicit override lost' }
} finally {
    $env:AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY = $saved
    Remove-Item $tempDir -Recurse -Force
}
Write-Host 'PASS: PowerShell env persistence and embedded WASM forwarding, with independent image registry'
