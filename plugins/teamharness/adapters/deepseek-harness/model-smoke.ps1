param(
    [Parameter(Mandatory = $false)]
    [string]$DshRoot
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$AdapterDir = [IO.Path]::GetFullPath($PSScriptRoot)
$TeamHarnessDir = [IO.Path]::GetFullPath((Join-Path $AdapterDir '..\..'))
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $AdapterDir '..\..\..\..'))
if ([string]::IsNullOrWhiteSpace($DshRoot)) {
    $DshRoot = Join-Path (Split-Path -Parent $RepoRoot) 'deepseek-harness-rc2'
}
$DshRoot = [IO.Path]::GetFullPath($DshRoot)

if ([string]::IsNullOrWhiteSpace($env:DEEPSEEK_API_KEY)) {
    throw 'DEEPSEEK_API_KEY is required for the real-model smoke test'
}

$ExpectedCommit = 'b150a551b8d465e31e418e1b2eaf5e79bbb7d28e'
$ActualCommit = (& git -C $DshRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $ActualCommit -ne $ExpectedCommit) {
    throw "Expected DeepSeek Harness $ExpectedCommit, found $ActualCommit"
}

$DshCli = Join-Path $DshRoot 'apps\cli\lib\bin.js'
if (-not (Test-Path -LiteralPath $DshCli -PathType Leaf)) {
    throw "DSH is not built: $DshCli is missing"
}

$Python = Get-Command python -ErrorAction Stop
$TempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$RunRoot = Join-Path $TempBase ("teamharness-dsh-model-" + [guid]::NewGuid().ToString('N'))
$ModelDshHome = Join-Path $RunRoot 'dsh-home'
$Workspace = Join-Path $RunRoot 'workspace'
$RoleSkillRoot = Join-Path $RunRoot 'role-skills'
$TranscriptPath = Join-Path $RunRoot 'transcript.txt'
$PackageName = 'agentteams-teamharness-dsh'
$Succeeded = $false

New-Item -ItemType Directory -Path $ModelDshHome, $Workspace | Out-Null

$PreviousDshHome = $env:DSH_HOME
$PreviousPluginDir = $env:AGENTTEAMS_PLUGIN_DIR
$PreviousRuntimeConfig = $env:TEAMHARNESS_RUNTIME_CONFIG
$PreviousWorkspace = $env:TEAMHARNESS_WORKSPACE
$PreviousPython = $env:TEAMHARNESS_PYTHON
$PreviousSkillRoot = $env:TEAMHARNESS_DSH_SKILL_ROOT
$PreviousSmokeReport = $env:TEAMHARNESS_DSH_SMOKE_REPORT

try {
    $env:DSH_HOME = $ModelDshHome
    $env:AGENTTEAMS_PLUGIN_DIR = $TeamHarnessDir
    $env:TEAMHARNESS_RUNTIME_CONFIG = Join-Path $AdapterDir 'fixtures\runtime.yaml'
    $env:TEAMHARNESS_WORKSPACE = $Workspace
    $env:TEAMHARNESS_PYTHON = $Python.Source
    $env:TEAMHARNESS_DSH_SKILL_ROOT = $RoleSkillRoot
    $env:TEAMHARNESS_DSH_SMOKE_REPORT = $null

    Push-Location $DshRoot
    try {
        & pnpm --dir $AdapterDir pack --pack-destination $RunRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness DeepSeek Harness adapter pack failed' }
        $PackageArchive = Get-ChildItem -LiteralPath $RunRoot -Filter '*.tgz' | Select-Object -First 1
        if ($null -eq $PackageArchive) { throw 'TeamHarness DeepSeek Harness adapter pack produced no archive' }

        & node $DshCli plugin --profile headless add $PackageArchive.FullName
        if ($LASTEXITCODE -ne 0) { throw 'DSH plugin add failed' }

        $InstalledPackageDir = Join-Path $ModelDshHome "profiles\headless\node_modules\$PackageName"
        & node (Join-Path $InstalledPackageDir 'prepare-skills.js') `
            --plugin-dir $TeamHarnessDir `
            --runtime-config $env:TEAMHARNESS_RUNTIME_CONFIG `
            --output $RoleSkillRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness role skill preparation failed' }

        $Task = @'
Read your TeamHarness runtime identity from the system context. Reply with exactly two whitespace-separated fields: TEAMHARNESS_DSH_MODEL_OK followed by your member.runtimeName. Do not run shell commands and do not add punctuation or explanation.
'@
        $Transcript = (& node $DshCli --profile headless $Task 2>&1 | Out-String)
        [IO.File]::WriteAllText($TranscriptPath, $Transcript)
        if ($LASTEXITCODE -ne 0) { throw "DSH real-model turn failed:`n$Transcript" }
        if ($Transcript -notmatch '(?m)^TEAMHARNESS_DSH_MODEL_OK\s+dsh-worker-a\s*$') {
            throw "DSH model did not return the injected runtime identity. Transcript kept at $TranscriptPath"
        }
    }
    finally {
        Pop-Location
    }

    Write-Output 'real model: PASS'
    Write-Output 'injected runtime identity observed: dsh-worker-a'
    $Succeeded = $true
}
finally {
    $env:DSH_HOME = $PreviousDshHome
    $env:AGENTTEAMS_PLUGIN_DIR = $PreviousPluginDir
    $env:TEAMHARNESS_RUNTIME_CONFIG = $PreviousRuntimeConfig
    $env:TEAMHARNESS_WORKSPACE = $PreviousWorkspace
    $env:TEAMHARNESS_PYTHON = $PreviousPython
    $env:TEAMHARNESS_DSH_SKILL_ROOT = $PreviousSkillRoot
    $env:TEAMHARNESS_DSH_SMOKE_REPORT = $PreviousSmokeReport

    $ResolvedRunRoot = [IO.Path]::GetFullPath($RunRoot)
    if (-not $ResolvedRunRoot.StartsWith($TempBase, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to clean model-smoke path outside the temp directory: $ResolvedRunRoot"
    }
    if ($Succeeded) {
        Remove-Item -LiteralPath $ResolvedRunRoot -Recurse -Force
    }
    else {
        Write-Host "Model-smoke artifacts kept for debugging: $ResolvedRunRoot"
    }
}
