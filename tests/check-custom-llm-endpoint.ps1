$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot '../install/agentteams-install.ps1'
$tokens = $null; $errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($installer, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$fn = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'New-OpenAICompatProvider' }, $true)
Invoke-Expression $fn.Extent.Text
function Write-Log {}
function Get-Msg { param($Key) $Key }
function Start-Sleep {}
$script:config = @{ ADMIN_USER='review'; ADMIN_PASSWORD='synthetic-password' }
function Invoke-RestMethod {
    param($Uri,$Method,$Body,$ContentType,$ErrorAction,$WebSession,$SessionVariable)
    if ($Uri.EndsWith('/session/login')) { return @{name='review'} }
    if ($Method -eq 'GET') { return [pscustomobject]@{data=[pscustomobject]@{version='7';extra='preserve';rawConfigs=[pscustomobject]@{extra='preserve'}}} }
    if ($Method -eq 'POST' -and $script:existing) {
        $exception = [Exception]::new('Conflict')
        $exception | Add-Member -NotePropertyName Response -NotePropertyValue ([pscustomobject]@{StatusCode=409})
        throw $exception
    }
    $script:calls += @{uri=$Uri;method=$Method;body=($Body | ConvertFrom-Json)}
}
foreach ($existing in @($false,$true)) {
    $script:existing=$existing
    foreach ($case in @(
        @('http://192.0.2.10:3000/v1','static','192.0.2.10:3000',3000,'http://192.0.2.10/v1','http'),
        @('https://192.0.2.10:8443/v1','static','192.0.2.10:8443',8443,'https://192.0.2.10/v1','https'),
        @('https://api.example.com:8443/v1','dns','api.example.com',8443,'https://api.example.com/v1','https'),
        @('https://api.example.com/v1','dns','api.example.com',443,'https://api.example.com/v1','https')
    )) {
        $script:calls=@()
        if (-not (New-OpenAICompatProvider -BaseUrl $case[0] -ApiKey synthetic)) { throw 'Configuration failed' }
        $service=$calls[0].body; $provider=$calls[1].body
        if ($service.type -ne $case[1] -or $service.domain -ne $case[2] -or $service.port -ne $case[3] -or $service.protocol -ne $case[5]) { throw ($service | ConvertTo-Json) }
        if ($provider.rawConfigs.openaiCustomServiceName -ne "openai-compat.$($case[1])" -or $provider.rawConfigs.openaiCustomUrl -ne $case[4] -or $provider.rawConfigs.openaiCustomServicePort -ne $case[3]) { throw ($provider | ConvertTo-Json) }
        if ($existing -and ($service.version -ne '7' -or $provider.version -ne '7' -or $service.extra -ne 'preserve' -or $provider.rawConfigs.extra -ne 'preserve')) { throw 'Upgrade lost existing fields or version' }
    }
}
$script:calls=@()
if (New-OpenAICompatProvider -BaseUrl 'http://[::1]:3000/v1' -ApiKey synthetic) { throw 'Unsupported IPv6 accepted' }
if ($calls.Count) { throw 'IPv6 caused Console writes' }
Write-Host 'PASS: PowerShell custom LLM IP/DNS, HTTP/HTTPS, port, upgrade/restart, IPv6 rejection'
