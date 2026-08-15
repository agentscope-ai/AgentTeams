$ErrorActionPreference = "Stop"

$rootDir = Split-Path -Parent $PSScriptRoot
$installerPath = Join-Path $rootDir "install/agentteams-install.ps1"
$installer = Get-Content -Raw $installerPath
$functionMatch = [regex]::Match(
    $installer,
    '(?ms)^function ConvertTo-MatrixAppServiceEnabledValue \{.*?^\}'
)

if (-not $functionMatch.Success) {
    throw "ConvertTo-MatrixAppServiceEnabledValue was not found in the Windows installer"
}

Invoke-Expression $functionMatch.Value

$cases = @(
    [PSCustomObject]@{ Input = "false"; Expected = "false" }
    [PSCustomObject]@{ Input = "False"; Expected = "false" }
    [PSCustomObject]@{ Input = "FALSE"; Expected = "false" }
    [PSCustomObject]@{ Input = "0"; Expected = "0" }
    [PSCustomObject]@{ Input = "true"; Expected = "true" }
)

foreach ($case in $cases) {
    $actual = ConvertTo-MatrixAppServiceEnabledValue $case.Input
    if ($actual -cne $case.Expected) {
        throw "Expected '$($case.Input)' to normalize to '$($case.Expected)', got '$actual'"
    }
}

Write-Host "PASS: Windows installer normalizes Matrix AppService enablement values"
