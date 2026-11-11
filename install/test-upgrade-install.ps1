$ErrorActionPreference = 'Stop'
$tokens = $null
$errors = $null
$source = Join-Path $PSScriptRoot 'agentteams-install.ps1'
$ast = [System.Management.Automation.Language.Parser]::ParseFile($source, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$definitions = $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)
foreach ($name in @('Load-UpgradeConfig', 'Prepare-UpgradeDashboard', 'Get-InstalledStorage', 'Recover-ExistingInstall', 'Step-Existing', 'New-EnvFile')) {
    Invoke-Expression ($definitions | Where-Object Name -eq $name | Select-Object -First 1).Extent.Text
}
function Write-Log { param($Message) }
function Get-Msg { param($Key) return $Key }
function Write-Error { param($Message) throw $Message }
function Read-Host { param($Prompt) $script:prompts++; return '' }
function Exit-Script { param($ExitCode) throw 'image-failed' }
function docker {
    $global:LASTEXITCODE = 0
    if ($args[0] -in @('image', 'pull')) { if ($script:failImages) { $global:LASTEXITCODE = 1 }; return }
    switch ("$($args[0]) $($args[1])") {
        'ps -a' { if (-not $script:orphan) { 'agentteams-controller'; 'agentteams-manager'; 'agentteams-worker-test' }; return }
        'ps --format' { 'agentteams-manager'; 'agentteams-worker-test'; return }
        'volume inspect' { return }
        'volume ls' { if ($script:failImages) { throw 'Storage must not be created before image preparation' }; return 'original-data' }
        'inspect agentteams-controller' {
            switch ($args[3]) {
                '{{json .Mounts}}' { return (@(@{Type='volume'; Name=$script:mountedData; Destination='/data'}, @{Type='bind'; Source='/original/work space'; Destination='/root/agentteams-fs/agents/manager'}) | ConvertTo-Json -Compress) }
                '{{json .Config.Env}}' { return ($script:saved | ConvertTo-Json -Compress) }
                '{{.Config.Image}}' { return 'registry.example/controller:old' }
            }
        }
    }
    throw "Unexpected engine operation: $($args -join ' ')"
}
function Reset-Fixture {
    Get-ChildItem Env:AGENTTEAMS_* | ForEach-Object { [Environment]::SetEnvironmentVariable($_.Name, $null, 'Process') }
    $script:InstallEnvKeys = @{}
    $script:config = @{}
    $script:RecoveredConfig = $null
    $script:AGENTTEAMS_REGISTRY = 'registry.example'
    $script:AGENTTEAMS_VERSION = 'new'
    $script:AGENTTEAMS_NON_INTERACTIVE = $false
    $script:AGENTTEAMS_UPGRADE = $false
    $script:prompts = 0
    $script:orphan = $false
    $script:mountedData = 'original-data'
    $script:Command = 'manager'
    $script:saved = @('AGENTTEAMS_ADMIN_USER=operator', 'AGENTTEAMS_ADMIN_PASSWORD=secret#with=equals', 'AGENTTEAMS_MANAGER_PASSWORD=manager-secret', 'AGENTTEAMS_REGISTRATION_TOKEN=registration-secret', 'AGENTTEAMS_MINIO_USER=storage-admin', 'AGENTTEAMS_MINIO_PASSWORD=storage-secret', 'AGENTTEAMS_MANAGER_GATEWAY_KEY=gateway-secret', 'AGENTTEAMS_MATRIX_APPSERVICE_ENABLED=false', 'AGENTTEAMS_DATA_DIR=original-data', 'AGENTTEAMS_WORKSPACE_DIR=/original/work space', 'AGENTTEAMS_DASHBOARD=0', 'AGENTTEAMS_DOCKER_PROXY=0')
    $script:saved | Set-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE
}
$directory = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory $directory | Out-Null
$script:AGENTTEAMS_ENV_FILE = Join-Path $directory 'manager.env'
try {
    Reset-Fixture
    Step-Existing
    if ($script:prompts -ne 1 -or -not $script:AGENTTEAMS_UPGRADE -or $env:AGENTTEAMS_UPGRADE_KEEP_ALL -ne '1') { throw 'Upgrade must ask once and preserve settings' }
    if ($script:config.ADMIN_PASSWORD -ne 'secret#with=equals' -or $script:config.WORKSPACE_DIR -ne '/original/work space' -or $script:config.DASHBOARD -ne '0' -or $script:config.DOCKER_PROXY -ne '0') { throw 'Saved configuration was changed' }

    Reset-Fixture
    $script:AGENTTEAMS_NON_INTERACTIVE = $true
    Step-Existing
    if ($script:prompts -ne 0) { throw 'Unattended upgrade must not prompt' }

    Reset-Fixture
    $script:mountedData = 'conflicting-data'
    $failed = $false
    try { Step-Existing } catch { $failed = $_.Exception.Message -eq 'upgrade.recovery_failed' }
    if (-not $failed -or $script:prompts -ne 0) { throw 'Storage mismatch must stop before confirmation' }

    Reset-Fixture
    $script:saved | Where-Object { $_ -notlike 'AGENTTEAMS_MANAGER_PASSWORD=*' } | Set-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE
    $failed = $false
    try { Step-Existing } catch { $failed = $_.Exception.Message -eq 'upgrade.recovery_failed' }
    if (-not $failed -or $script:prompts -ne 0) { throw 'Incomplete credentials must stop instead of generating a new identity' }

    Reset-Fixture
    Remove-Item -LiteralPath $script:AGENTTEAMS_ENV_FILE
    Recover-ExistingInstall
    if (Test-Path $script:AGENTTEAMS_ENV_FILE) { throw 'Recovery must not write before confirmation' }
    Step-Existing
    if ($script:config.DATA_DIR -ne 'original-data' -or $script:config.ADMIN_PASSWORD -ne 'secret#with=equals') { throw 'Recovery did not retain original data and credentials' }

    Reset-Fixture
    Remove-Item -LiteralPath $script:AGENTTEAMS_ENV_FILE
    $script:orphan = $true
    $failed = $false
    try { Recover-ExistingInstall } catch { $failed = $_.Exception.Message -eq 'upgrade.recovery_failed' }
    if (-not $failed) { throw 'Orphan data must not be removed or reused with new credentials' }

    Reset-Fixture
    $script:config = @{DASHBOARD_VERSION='v1.3.1'; DASHBOARD_IMAGE='registry.example/agentteams/agentteams-dashboard:v1.3.1'}
    Prepare-UpgradeDashboard
    if ($script:config.DASHBOARD_VERSION -ne 'v1.4.0' -or $script:config.DASHBOARD -ne '1') { throw 'Old default Dashboard was not upgraded' }
    $script:config = @{DASHBOARD_VERSION='v1.3.1'; DASHBOARD_IMAGE='custom/dashboard:pinned'; DASHBOARD='0'}
    Prepare-UpgradeDashboard
    if ($script:config.DASHBOARD_IMAGE -ne 'custom/dashboard:pinned' -or $script:config.DASHBOARD -ne '0') { throw 'Custom Dashboard configuration was changed' }
    $script:InstallEnvKeys.AGENTTEAMS_DASHBOARD_VERSION = $true
    $script:config = @{DASHBOARD_VERSION='v1.3.1'}
    Prepare-UpgradeDashboard
    if ($script:config.DASHBOARD_VERSION -ne 'v1.3.1') { throw 'Explicit Dashboard version was changed' }
    $text = Get-Content -LiteralPath $source -Raw
    $start = $text.IndexOf('    # Pull images (skip if already exists locally for local build tags).')
    $end = $text.IndexOf('    # --- Pre-upgrade: extract Matrix passwords', $start)
    $imagePhase = $text.Substring($start, $end - $start)
    Reset-Fixture
    Remove-Item -LiteralPath $script:AGENTTEAMS_ENV_FILE
    $config = $script:config
    $script:AGENTTEAMS_UPGRADE = $false
    $script:AGENTTEAMS_USE_EMBEDDED = '0'
    $managerImage = 'agentteams/manager:test'
    $script:failImages = $true
    $failed = $false
    try { Invoke-Expression $imagePhase } catch { if ($_.Exception.Message -ne 'image-failed') { throw }; $failed = $true }
    if (-not $failed -or (Test-Path $script:AGENTTEAMS_ENV_FILE)) { throw 'A failed first download must leave no new configuration or storage' }
    foreach ($fail in @($true, $false)) {
        Reset-Fixture
        Add-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE -Value 'AGENTTEAMS_FS_ENDPOINT=https://storage.example'
        Add-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE -Value 'AGENTTEAMS_CUSTOM_OPTION=with space#and=equals'
        $original = Get-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE -Raw
        Load-UpgradeConfig
        $config = $script:config
        $script:AGENTTEAMS_UPGRADE = $true
        $script:AGENTTEAMS_USE_EMBEDDED = '0'
        $managerImage = 'agentteams/manager:test'
        $script:failImages = $fail
        $backup = $null
        $failed = $false
        try { Invoke-Expression $imagePhase } catch { if ($_.Exception.Message -ne 'image-failed') { throw }; $failed = $true }
        $backups = @(Get-ChildItem -LiteralPath $directory -Filter 'manager.env.before-upgrade-*')
        if ($fail) {
            if (-not $failed -or $backups.Count -or (Get-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE -Raw) -cne $original) { throw 'Failed image preparation changed saved configuration' }
        } else {
            if ($failed -or $backups.Count -ne 1 -or (Get-Content -LiteralPath $backups[0].FullName -Raw) -cne $original) { throw 'Original configuration was not backed up' }
            $updated = Get-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE
            foreach ($line in @('AGENTTEAMS_FS_ENDPOINT=https://storage.example', 'AGENTTEAMS_CUSTOM_OPTION=with space#and=equals', 'AGENTTEAMS_ADMIN_PASSWORD=secret#with=equals')) {
                if ($updated -notcontains $line) { throw 'Saved settings were lost during upgrade' }
            }
        }
    }
    Reset-Fixture
    Load-UpgradeConfig
    $config = $script:config
    $original = Get-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE -Raw
    $script:AGENTTEAMS_UPGRADE = $true
    $script:failImages = $false
    function Copy-Item {
        [CmdletBinding()]
        param($LiteralPath, $Destination)
        Microsoft.PowerShell.Utility\Write-Error 'synthetic backup failure'
    }
    $failed = $false
    try {
        $ErrorActionPreference = 'Continue'
        try { Invoke-Expression $imagePhase } catch { $failed = $true }
    } finally {
        $ErrorActionPreference = 'Stop'
        Remove-Item Function:Copy-Item
    }
    if (-not $failed -or (Get-Content -LiteralPath $script:AGENTTEAMS_ENV_FILE -Raw) -cne $original) { throw 'A failed backup must stop before replacing configuration' }

    $workerBlock = $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.IfStatementAst] -and $node.Clauses[0].Item1.Extent.Text -eq '$isOldArch -and $script:UPGRADE_EXISTING_WORKERS' }, $true) | Select-Object -First 1
    if (-not $workerBlock) { throw 'Worker migration block missing' }
    $isOldArch = $false
    $script:UPGRADE_EXISTING_WORKERS = @('agentteams-worker-test')
    # The engine mock rejects stop/rm: normal upgrades must never call them for Workers.
    Invoke-Expression $workerBlock.Extent.Text
    Write-Host 'PowerShell upgrade tests passed (13 scenarios)'
} finally {
    Remove-Item -LiteralPath $directory -Recurse -Force
}
