$ErrorActionPreference = 'Stop'

$Installer = Join-Path $PSScriptRoot '..\install\agentteams-install.ps1'
$Tokens = $null
$Errors = $null
$Ast = [System.Management.Automation.Language.Parser]::ParseFile(
    (Resolve-Path $Installer),
    [ref]$Tokens,
    [ref]$Errors
)
if ($Errors.Count -gt 0) {
    throw "PowerShell installer has parse errors: $($Errors -join '; ')"
}

$FunctionAst = $Ast.Find({
    param($Node)
    $Node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
        $Node.Name -eq 'Update-AgentTeamsKnownStableVersion'
}, $true)
if ($null -eq $FunctionAst) {
    throw 'Update-AgentTeamsKnownStableVersion was not found'
}
Invoke-Expression $FunctionAst.Extent.Text

$script:ProbeCalls = 0
function Invoke-RestMethod {
    param($Uri, $Headers, $TimeoutSec, $ErrorAction)
    $script:ProbeCalls++
    return [pscustomobject]@{ tag_name = 'v1.2.4' }
}

$script:AGENTTEAMS_VERSION = 'latest'
$script:AGENTTEAMS_KNOWN_STABLE_VERSION = 'v1.2.3'
Update-AgentTeamsKnownStableVersion
if ($script:ProbeCalls -ne 1 -or $script:AGENTTEAMS_KNOWN_STABLE_VERSION -ne 'v1.2.4') {
    throw 'PowerShell latest probe did not update the stable Controller feature-gate version'
}

$script:AGENTTEAMS_VERSION = 'v1.2.3'
$script:ProbeCalls = 0
Update-AgentTeamsKnownStableVersion
if ($script:ProbeCalls -ne 0) {
    throw 'A pinned AgentTeams version must not query or replace the selected release'
}

Write-Output 'PASS: PowerShell latest release probe updates the DSH feature gate'
