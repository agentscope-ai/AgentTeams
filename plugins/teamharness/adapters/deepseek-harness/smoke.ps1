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

$ExpectedCommit = 'b150a551b8d465e31e418e1b2eaf5e79bbb7d28e'
$ActualCommit = (& git -C $DshRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $ActualCommit -ne $ExpectedCommit) {
    throw "Expected DeepSeek Harness $ExpectedCommit, found $ActualCommit"
}

$DshCli = Join-Path $DshRoot 'apps\cli\lib\bin.js'
if (-not (Test-Path -LiteralPath $DshCli -PathType Leaf)) {
    throw "DSH is not built: $DshCli is missing. Run pnpm install and pnpm run build in $DshRoot"
}

$Python = Get-Command python -ErrorAction Stop
$TempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$RunRoot = Join-Path $TempBase ("teamharness-dsh-adapter-" + [guid]::NewGuid().ToString('N'))
$AdapterDshHome = Join-Path $RunRoot 'dsh-home'
$Workspace = Join-Path $RunRoot 'workspace'
$RoleSkillRoot = Join-Path $RunRoot 'role-skills'
$StandaloneSkillRoot = Join-Path $RunRoot 'standalone-role-skills'
$StandaloneRuntimeConfig = Join-Path $RunRoot 'standalone-runtime.yaml'
$ResolvedConfig = Join-Path $RunRoot 'resolved.cordis.yml'
$UpdatedConfig = Join-Path $RunRoot 'updated.cordis.yml'
$AfterRemoveConfig = Join-Path $RunRoot 'after-remove.cordis.yml'
$ReportPath = Join-Path $RunRoot 'report.json'
$ProfileName = 'teamharness-smoke'
$PackageName = 'agentteams-teamharness-dsh'
$Succeeded = $false

$SmokeArtifactDir = Join-Path $Workspace 'shared\deepseek-harness\artifacts'
New-Item -ItemType Directory -Path $AdapterDshHome, $Workspace, $SmokeArtifactDir | Out-Null
[IO.File]::WriteAllText((Join-Path $SmokeArtifactDir 'smoke.txt'), "TeamHarness DSH smoke`n")

$PreviousDshHome = $env:DSH_HOME
$PreviousPluginDir = $env:AGENTTEAMS_PLUGIN_DIR
$PreviousRuntimeConfig = $env:TEAMHARNESS_RUNTIME_CONFIG
$PreviousWorkspace = $env:TEAMHARNESS_WORKSPACE
$PreviousPython = $env:TEAMHARNESS_PYTHON
$PreviousSmokeReport = $env:TEAMHARNESS_DSH_SMOKE_REPORT
$PreviousSkillRoot = $env:TEAMHARNESS_DSH_SKILL_ROOT
$PreviousExpectedRole = $env:TEAMHARNESS_DSH_EXPECTED_ROLE
$PreviousExpectMessageTool = $env:TEAMHARNESS_DSH_EXPECT_MESSAGE_TOOL
$PreviousExpectTeamContract = $env:TEAMHARNESS_DSH_EXPECT_TEAM_CONTRACT
$PreviousDshModel = $env:TEAMHARNESS_DSH_MODEL

try {
    $env:DSH_HOME = $AdapterDshHome
    $env:AGENTTEAMS_PLUGIN_DIR = $TeamHarnessDir
    $env:TEAMHARNESS_RUNTIME_CONFIG = Join-Path $AdapterDir 'fixtures\runtime.yaml'
    $env:TEAMHARNESS_WORKSPACE = $Workspace
    $env:TEAMHARNESS_PYTHON = $Python.Source
    $env:TEAMHARNESS_DSH_SMOKE_REPORT = $ReportPath
    $env:TEAMHARNESS_DSH_SKILL_ROOT = $RoleSkillRoot
    $env:TEAMHARNESS_DSH_EXPECTED_ROLE = 'worker'
    $env:TEAMHARNESS_DSH_EXPECT_MESSAGE_TOOL = 'false'
    $env:TEAMHARNESS_DSH_EXPECT_TEAM_CONTRACT = 'true'
    $env:TEAMHARNESS_DSH_MODEL = 'teamharness-smoke-model'

    Push-Location $DshRoot
    try {
        & pnpm --dir $AdapterDir pack --pack-destination $RunRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness DeepSeek Harness adapter pack failed' }
        $PackageArchive = Get-ChildItem -LiteralPath $RunRoot -Filter '*.tgz' | Select-Object -First 1
        if ($null -eq $PackageArchive) { throw 'TeamHarness DeepSeek Harness adapter pack produced no archive' }

        & node $DshCli plugin --profile $ProfileName add $PackageArchive.FullName
        if ($LASTEXITCODE -ne 0) { throw 'DSH plugin add failed' }
        $InstalledPackageDir = Join-Path $AdapterDshHome "profiles\$ProfileName\node_modules\$PackageName"
        & node (Join-Path $InstalledPackageDir 'prepare-skills.js') --plugin-dir $TeamHarnessDir --runtime-config $env:TEAMHARNESS_RUNTIME_CONFIG --output $RoleSkillRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness role skill preparation failed' }

        $StandaloneRuntime = (Get-Content -Raw $env:TEAMHARNESS_RUNTIME_CONFIG).Replace('role: worker', 'role: standalone')
        $StandaloneRuntime = [regex]::Replace($StandaloneRuntime, '(?ms)^team:\r?\n.*?(?=^member:)', '')
        [IO.File]::WriteAllText($StandaloneRuntimeConfig, $StandaloneRuntime)
        $StandalonePreparation = (& node (Join-Path $InstalledPackageDir 'prepare-skills.js') --plugin-dir $TeamHarnessDir --runtime-config $StandaloneRuntimeConfig --output $StandaloneSkillRoot | Out-String)
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness standalone role skill preparation failed' }
        if (-not $StandalonePreparation.Contains('"role":"worker"') -or -not (Test-Path -LiteralPath (Join-Path $StandaloneSkillRoot 'task-execution') -PathType Container)) {
            throw "Standalone runtime did not receive worker skills: $StandalonePreparation"
        }

        $WorkerRuntimeConfig = $env:TEAMHARNESS_RUNTIME_CONFIG
        $WorkerSkillRoot = $env:TEAMHARNESS_DSH_SKILL_ROOT
        $WorkerExpectedRole = $env:TEAMHARNESS_DSH_EXPECTED_ROLE
        $WorkerExpectMessageTool = $env:TEAMHARNESS_DSH_EXPECT_MESSAGE_TOOL
        $WorkerExpectTeamContract = $env:TEAMHARNESS_DSH_EXPECT_TEAM_CONTRACT
        $StandaloneRuntimeExit = 1
        try {
            $env:TEAMHARNESS_RUNTIME_CONFIG = $StandaloneRuntimeConfig
            $env:TEAMHARNESS_DSH_SKILL_ROOT = $StandaloneSkillRoot
            $env:TEAMHARNESS_DSH_EXPECTED_ROLE = 'standalone'
            $env:TEAMHARNESS_DSH_EXPECT_MESSAGE_TOOL = 'true'
            $env:TEAMHARNESS_DSH_EXPECT_TEAM_CONTRACT = 'false'
            & node $DshCli --profile $ProfileName
            $StandaloneRuntimeExit = $LASTEXITCODE
        }
        finally {
            $env:TEAMHARNESS_RUNTIME_CONFIG = $WorkerRuntimeConfig
            $env:TEAMHARNESS_DSH_SKILL_ROOT = $WorkerSkillRoot
            $env:TEAMHARNESS_DSH_EXPECTED_ROLE = $WorkerExpectedRole
            $env:TEAMHARNESS_DSH_EXPECT_MESSAGE_TOOL = $WorkerExpectMessageTool
            $env:TEAMHARNESS_DSH_EXPECT_TEAM_CONTRACT = $WorkerExpectTeamContract
        }
        if ($StandaloneRuntimeExit -ne 0) { throw 'TeamHarness standalone DSH runtime smoke failed' }

        $Dump = (& node $DshCli --profile $ProfileName --dump-config | Out-String)
        if ($LASTEXITCODE -ne 0) { throw 'DSH config dump failed' }
        [IO.File]::WriteAllText($ResolvedConfig, $Dump)
        if (-not $Dump.Contains($PackageName) -or -not $Dump.Contains('teamharness-mcp')) {
            throw 'Installed DSH profile does not contain the TeamHarness bundle rows'
        }

        & node $DshCli --profile $ProfileName
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness DSH runtime smoke failed' }
        if (-not (Test-Path -LiteralPath $ReportPath -PathType Leaf)) { throw 'TeamHarness DSH smoke produced no report' }

        & node $DshCli plugin --profile $ProfileName add $PackageArchive.FullName
        if ($LASTEXITCODE -ne 0) { throw 'DSH plugin update failed' }
        & node (Join-Path $InstalledPackageDir 'prepare-skills.js') --plugin-dir $TeamHarnessDir --runtime-config $env:TEAMHARNESS_RUNTIME_CONFIG --output $RoleSkillRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness role skill update failed' }
        $UpdatedDump = (& node $DshCli --profile $ProfileName --dump-config | Out-String)
        if ($LASTEXITCODE -ne 0) { throw 'DSH config dump after update failed' }
        [IO.File]::WriteAllText($UpdatedConfig, $UpdatedDump)
        $McpRows = [regex]::Matches($UpdatedDump, '(?m)^- id: teamharness-mcp\s*$').Count
        if ($McpRows -ne 1) { throw "DSH plugin update left $McpRows TeamHarness MCP rows" }

        & node $DshCli --profile $ProfileName
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness DSH runtime smoke after update failed' }

        & node $DshCli plugin --profile $ProfileName remove $PackageName
        if ($LASTEXITCODE -ne 0) { throw 'DSH plugin remove failed' }

        $AfterRemoveDump = (& node $DshCli --profile $ProfileName --dump-config | Out-String)
        if ($LASTEXITCODE -ne 0) { throw 'DSH config dump after remove failed' }
        [IO.File]::WriteAllText($AfterRemoveConfig, $AfterRemoveDump)
        if ($AfterRemoveDump.Contains($PackageName) -or $AfterRemoveDump.Contains('teamharness-mcp')) {
            throw 'TeamHarness bundle rows remain after uninstall'
        }
    }
    finally {
        Pop-Location
    }

    Get-Content -Raw $ReportPath
    $Succeeded = $true
}
finally {
    $env:DSH_HOME = $PreviousDshHome
    $env:AGENTTEAMS_PLUGIN_DIR = $PreviousPluginDir
    $env:TEAMHARNESS_RUNTIME_CONFIG = $PreviousRuntimeConfig
    $env:TEAMHARNESS_WORKSPACE = $PreviousWorkspace
    $env:TEAMHARNESS_PYTHON = $PreviousPython
    $env:TEAMHARNESS_DSH_SMOKE_REPORT = $PreviousSmokeReport
    $env:TEAMHARNESS_DSH_SKILL_ROOT = $PreviousSkillRoot
    $env:TEAMHARNESS_DSH_EXPECTED_ROLE = $PreviousExpectedRole
    $env:TEAMHARNESS_DSH_EXPECT_MESSAGE_TOOL = $PreviousExpectMessageTool
    $env:TEAMHARNESS_DSH_EXPECT_TEAM_CONTRACT = $PreviousExpectTeamContract
    $env:TEAMHARNESS_DSH_MODEL = $PreviousDshModel

    $ResolvedRunRoot = [IO.Path]::GetFullPath($RunRoot)
    if (-not $ResolvedRunRoot.StartsWith($TempBase, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to clean adapter path outside the temp directory: $ResolvedRunRoot"
    }
    if ($Succeeded) {
        Remove-Item -LiteralPath $ResolvedRunRoot -Recurse -Force
    }
    else {
        Write-Host "Adapter artifacts kept for debugging: $ResolvedRunRoot"
    }
}
