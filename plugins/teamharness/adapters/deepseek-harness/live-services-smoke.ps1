param(
    [Parameter(Mandatory = $false)]
    [string]$DshRoot,
    [Parameter(Mandatory = $false)]
    [string]$Namespace = 'agentteams',
    [Parameter(Mandatory = $true)]
    [string]$WorkerName,
    [Parameter(Mandatory = $false)]
    [string]$McBinary
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$AdapterDir = [IO.Path]::GetFullPath($PSScriptRoot)
$TeamHarnessDir = [IO.Path]::GetFullPath((Join-Path $AdapterDir '..\..'))
$RepoRoot = [IO.Path]::GetFullPath((Join-Path $AdapterDir '..\..\..\..'))
. (Join-Path $RepoRoot 'deepseek-harness/tests/lib/test-helpers.ps1')
if ([string]::IsNullOrWhiteSpace($DshRoot)) {
    $DshRoot = Join-Path (Split-Path -Parent $RepoRoot) 'deepseek-harness-rc2'
}
$DshRoot = [IO.Path]::GetFullPath($DshRoot)
$DshCli = Join-Path $DshRoot 'apps\cli\lib\bin.js'
$Python = Get-Command python -ErrorAction Stop
$Kubectl = Get-Command kubectl -ErrorAction Stop

$Worker = & $Kubectl.Source get worker $WorkerName -n $Namespace -o json | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $Worker.status.phase -ne 'Running') {
    throw "Worker $WorkerName is not Running"
}
$PodName = "agentteams-worker-$WorkerName"
$Pod = & $Kubectl.Source get pod $PodName -n $Namespace -o json | ConvertFrom-Json
if ($LASTEXITCODE -ne 0) { throw "Cannot read pod $PodName" }

$MatrixToken = Get-PodEnv $Pod 'AGENTTEAMS_WORKER_MATRIX_TOKEN'
$MatrixDomain = Get-PodEnv $Pod 'AGENTTEAMS_MATRIX_DOMAIN'
$RoomId = Get-PodEnv $Pod 'AGENTTEAMS_WORKER_ROOM_ID'
$MatrixUserId = [string]$Worker.status.matrixUserID
$StorageAccessKey = Get-PodEnv $Pod 'AGENTTEAMS_FS_ACCESS_KEY'
$StorageSecretKey = Get-PodEnv $Pod 'AGENTTEAMS_FS_SECRET_KEY'
$StorageBucket = Get-PodEnv $Pod 'AGENTTEAMS_FS_BUCKET'
$StoragePrefix = Get-PodEnv $Pod 'AGENTTEAMS_STORAGE_PREFIX'

$TempBase = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$RunRoot = Join-Path $TempBase ("teamharness-dsh-live-" + [guid]::NewGuid().ToString('N'))
$LiveDshHome = Join-Path $RunRoot 'dsh-home'
$Workspace = Join-Path $RunRoot 'workspace'
$RoleSkillRoot = Join-Path $RunRoot 'role-skills'
$ReportPath = Join-Path $RunRoot 'report.json'
$RuntimePath = Join-Path $RunRoot 'runtime.yaml'
$McPath = if ([string]::IsNullOrWhiteSpace($McBinary)) {
    Join-Path $RunRoot 'mc.exe'
} else {
    [IO.Path]::GetFullPath($McBinary)
}
$Marker = 'TEAMHARNESS_DSH_LIVE_' + [guid]::NewGuid().ToString('N')
$ArtifactRelativePath = "shared\deepseek-harness\live\$Marker.txt"
$ArtifactPath = Join-Path $Workspace $ArtifactRelativePath
$MatrixPort = Get-FreeTcpPort
$MinioPort = Get-FreeTcpPort
$MatrixOut = Join-Path $RunRoot 'matrix-port-forward.log'
$MatrixErr = Join-Path $RunRoot 'matrix-port-forward.err.log'
$MinioOut = Join-Path $RunRoot 'minio-port-forward.log'
$MinioErr = Join-Path $RunRoot 'minio-port-forward.err.log'
$ProfileName = 'teamharness-live'
$PackageName = 'agentteams-teamharness-dsh'
$Succeeded = $false
$MatrixForward = $null
$MinioForward = $null

New-Item -ItemType Directory -Path $LiveDshHome, (Split-Path -Parent $ArtifactPath) | Out-Null
[IO.File]::WriteAllText($ArtifactPath, $Marker)

$RuntimeYaml = @"
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig
metadata:
  generation: 1
member:
  name: $WorkerName
  runtimeName: $WorkerName
  role: team_leader
  runtime: deepseek-harness
  matrixUserId: '$MatrixUserId'
  personalRoomId: '$RoomId'
team:
  name: ui-team-01
  storageId: ui-team-01
  teamRoomId: '$RoomId'
  leaderName: $WorkerName
  leaderRuntimeName: $WorkerName
storage:
  provider: minio
  bucket: $StorageBucket
  teamPrefix: teams/ui-team-01
  sharedPrefix: teams/ui-team-01/shared
  globalSharedPrefix: shared
  memberPrefix: agents/$WorkerName
desired:
  state: Running
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
  storageAccessKeyEnv: AGENTTEAMS_FS_ACCESS_KEY
  storageSecretKeyEnv: AGENTTEAMS_FS_SECRET_KEY
"@
[IO.File]::WriteAllText($RuntimePath, $RuntimeYaml)

$Previous = @{}
foreach ($Name in @(
    'DSH_HOME', 'PATH', 'AGENTTEAMS_PLUGIN_DIR', 'TEAMHARNESS_RUNTIME_CONFIG',
    'TEAMHARNESS_WORKSPACE', 'TEAMHARNESS_PYTHON', 'TEAMHARNESS_DSH_SKILL_ROOT',
    'TEAMHARNESS_DSH_SMOKE_REPORT', 'TEAMHARNESS_DSH_LIVE_REPORT',
    'TEAMHARNESS_DSH_LIVE_MARKER', 'TEAMHARNESS_DSH_LIVE_ROOM_ID',
    'AGENTTEAMS_MATRIX_URL', 'AGENTTEAMS_MATRIX_DOMAIN', 'AGENTTEAMS_MATRIX_USER_ID',
    'AGENTTEAMS_WORKER_MATRIX_TOKEN', 'AGENTTEAMS_FS_ENDPOINT',
    'AGENTTEAMS_FS_ACCESS_KEY', 'AGENTTEAMS_FS_SECRET_KEY', 'AGENTTEAMS_FS_BUCKET',
    'AGENTTEAMS_STORAGE_PREFIX'
)) { $Previous[$Name] = [Environment]::GetEnvironmentVariable($Name) }

try {
    $MatrixForward = Start-Process -FilePath $Kubectl.Source -ArgumentList @(
        'port-forward', '-n', $Namespace, 'service/agentteams-tuwunel', "${MatrixPort}:6167"
    ) -PassThru -WindowStyle Hidden -RedirectStandardOutput $MatrixOut -RedirectStandardError $MatrixErr
    $MinioForward = Start-Process -FilePath $Kubectl.Source -ArgumentList @(
        'port-forward', '-n', $Namespace, 'service/agentteams-minio', "${MinioPort}:9000"
    ) -PassThru -WindowStyle Hidden -RedirectStandardOutput $MinioOut -RedirectStandardError $MinioErr
    Wait-TcpPort $MatrixPort
    Wait-TcpPort $MinioPort

    if (-not (Test-Path -LiteralPath $McPath -PathType Leaf)) {
        Invoke-WebRequest -Uri 'https://dl.min.io/client/mc/release/windows-amd64/mc.exe' -OutFile $McPath
    }
    & $McPath --version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Downloaded MinIO client did not start' }

    $env:DSH_HOME = $LiveDshHome
    $env:PATH = "$(Split-Path -Parent $McPath);$($Previous['PATH'])"
    $env:AGENTTEAMS_PLUGIN_DIR = $TeamHarnessDir
    $env:TEAMHARNESS_RUNTIME_CONFIG = $RuntimePath
    $env:TEAMHARNESS_WORKSPACE = $Workspace
    $env:TEAMHARNESS_PYTHON = $Python.Source
    $env:TEAMHARNESS_DSH_SKILL_ROOT = $RoleSkillRoot
    $env:TEAMHARNESS_DSH_SMOKE_REPORT = $null
    $env:TEAMHARNESS_DSH_LIVE_REPORT = $ReportPath
    $env:TEAMHARNESS_DSH_LIVE_MARKER = $Marker
    $env:TEAMHARNESS_DSH_LIVE_ROOM_ID = $RoomId
    $env:AGENTTEAMS_MATRIX_URL = "http://127.0.0.1:$MatrixPort"
    $env:AGENTTEAMS_MATRIX_DOMAIN = $MatrixDomain
    $env:AGENTTEAMS_MATRIX_USER_ID = $MatrixUserId
    $env:AGENTTEAMS_WORKER_MATRIX_TOKEN = $MatrixToken
    $env:AGENTTEAMS_FS_ENDPOINT = "http://127.0.0.1:$MinioPort"
    $env:AGENTTEAMS_FS_ACCESS_KEY = $StorageAccessKey
    $env:AGENTTEAMS_FS_SECRET_KEY = $StorageSecretKey
    $env:AGENTTEAMS_FS_BUCKET = $StorageBucket
    $env:AGENTTEAMS_STORAGE_PREFIX = $StoragePrefix

    Push-Location $DshRoot
    try {
        & pnpm --dir $AdapterDir pack --pack-destination $RunRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness DeepSeek Harness adapter pack failed' }
        $PackageArchive = Get-ChildItem -LiteralPath $RunRoot -Filter '*.tgz' | Select-Object -First 1
        & node $DshCli plugin --profile $ProfileName add $PackageArchive.FullName
        if ($LASTEXITCODE -ne 0) { throw 'DSH plugin add failed' }
        $InstalledPackageDir = Join-Path $LiveDshHome "profiles\$ProfileName\node_modules\$PackageName"
        & node (Join-Path $InstalledPackageDir 'prepare-skills.js') --plugin-dir $TeamHarnessDir --runtime-config $RuntimePath --output $RoleSkillRoot
        if ($LASTEXITCODE -ne 0) { throw 'TeamHarness role skill preparation failed' }
        & node $DshCli --profile $ProfileName
        if ($LASTEXITCODE -ne 0) { throw 'DSH live-service probe failed' }
    }
    finally { Pop-Location }

    $Report = Get-Content -Raw $ReportPath | ConvertFrom-Json
    if (@($Report.failed).Count -ne 0) { throw "DSH live probe checks failed: $(@($Report.failed) -join ', ')" }

    $EncodedRoom = [Uri]::EscapeDataString($RoomId)
    $EncodedEvent = [Uri]::EscapeDataString([string]$Report.message.messageId)
    $Messages = Invoke-MatrixJson 'GET' "http://127.0.0.1:$MatrixPort/_matrix/client/v3/rooms/$EncodedRoom/messages?dir=b&limit=50" $MatrixToken
    $ObservedEvent = @($Messages.chunk) | Where-Object {
        [string]$_.event_id -eq [string]$Report.message.messageId -and [string]$_.content.body -eq $Marker
    } | Select-Object -First 1
    if ($null -eq $ObservedEvent) { throw 'Matrix read-back did not find the sent event and marker' }

    $RemotePath = [string]$Report.push.remotePath
    if (-not $RemotePath.StartsWith("$StoragePrefix/teams/ui-team-01/shared/deepseek-harness/live/", [StringComparison]::Ordinal)) {
        throw "Refusing to inspect unexpected storage path: $RemotePath"
    }
    $AliasUrl = "http://$([Uri]::EscapeDataString($StorageAccessKey)):$([Uri]::EscapeDataString($StorageSecretKey))@127.0.0.1:$MinioPort"
    $env:MC_HOST_agentteams = $AliasUrl
    $Stored = (& $McPath cat $RemotePath | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $Stored -ne $Marker) { throw 'MinIO read-back did not match the pushed marker' }

    & $McPath rm $RemotePath | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Failed to remove live-test MinIO artifact' }
    Invoke-MatrixJson 'PUT' "http://127.0.0.1:$MatrixPort/_matrix/client/v3/rooms/$EncodedRoom/redact/$EncodedEvent/$([guid]::NewGuid().ToString('N'))" $MatrixToken @{ reason = 'TeamHarness DSH live smoke cleanup' } | Out-Null

    Write-Output 'Matrix delivery + public read-back: PASS'
    Write-Output 'MinIO push/stat + public read-back: PASS'
    $Succeeded = $true
}
finally {
    foreach ($Entry in $Previous.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable([string]$Entry.Key, [string]$Entry.Value)
    }
    Remove-Item Env:MC_HOST_agentteams -ErrorAction SilentlyContinue
    foreach ($Process in @($MatrixForward, $MinioForward)) {
        if ($null -ne $Process -and -not $Process.HasExited) { Stop-Process -Id $Process.Id -Force }
    }
    if ($Succeeded) {
        Remove-Item -LiteralPath $RunRoot -Recurse -Force
    }
    else {
        Write-Host "Live-service artifacts kept for debugging: $RunRoot"
    }
}
