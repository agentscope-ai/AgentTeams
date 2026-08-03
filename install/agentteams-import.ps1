# agentteams-import.ps1 - Import Worker/Team/Human resources into AgentTeams
#
# Thin shell that delegates to the `agt` CLI inside the Manager container.
# Supports ZIP packages, remote packages (nacos://, http://), and YAML files.
#
# Usage:
#   .\agentteams-import.ps1 worker -Name <name> -Zip <path-or-url>
#   .\agentteams-import.ps1 worker -Name <name> -Package <nacos://...> [-Model MODEL]
#   .\agentteams-import.ps1 worker -Name <name>                        # auto-imports package <name>
#   .\agentteams-import.ps1 worker -Name <name> -Model MODEL [-Skills s1,s2] [-McpServers m1,m2]
#   .\agentteams-import.ps1 -File <resource.yaml> [-Prune] [-DryRun]

param(
    [Parameter(Position = 0)]
    [string]$ResourceType = "",

    [string]$Name = "",
    [string]$Model = "",
    [string]$Zip = "",
    [string]$Package = "",
    [string]$Skills = "",
    [string]$McpServers = "",
    [string]$Runtime = "",
    [string]$File = "",
    [switch]$Prune,
    [switch]$DryRun,
    [switch]$Yes
)

$ErrorActionPreference = "Stop"

# ============================================================
# Detect container runtime
# ============================================================

$ContainerCmd = ""
try { $null = & docker info 2>$null; $ContainerCmd = "docker" } catch {}
if (-not $ContainerCmd) {
    try { $null = & podman info 2>$null; $ContainerCmd = "podman" } catch {}
}
if (-not $ContainerCmd) {
    Write-Host "[AgentTeams Import ERROR] Neither docker nor podman found." -ForegroundColor Red
    Write-Host ""
    Write-Host "Docker is required to run AgentTeams. Install Docker Desktop first, then install AgentTeams:" -ForegroundColor Yellow
    Write-Host "  Set-ExecutionPolicy Bypass -Scope Process -Force; `$wc=New-Object Net.WebClient; `$wc.Encoding=[Text.Encoding]::UTF8; iex `$wc.DownloadString('https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.ps1')"
    exit 1
}

# Verify Manager container
$mgrRunning = & $ContainerCmd ps --filter "name=agentteams-manager" --format "{{.Names}}" 2>$null
if ($mgrRunning -notmatch "agentteams-manager") {
    Write-Host "[AgentTeams Import ERROR] agentteams-manager container is not running." -ForegroundColor Red
    Write-Host ""
    # Check if the container exists but is stopped
    $mgrExists = & $ContainerCmd ps -a --filter "name=agentteams-manager" --format "{{.Names}}" 2>$null
    if ($mgrExists -match "agentteams-manager") {
        Write-Host "The agentteams-manager container exists but is stopped. Start it with:" -ForegroundColor Yellow
        Write-Host "  $ContainerCmd start agentteams-manager"
    } else {
        Write-Host "AgentTeams does not appear to be installed. Install it first:" -ForegroundColor Yellow
        Write-Host "  Set-ExecutionPolicy Bypass -Scope Process -Force; `$wc=New-Object Net.WebClient; `$wc.Encoding=[Text.Encoding]::UTF8; iex `$wc.DownloadString('https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.ps1')"
    }
    exit 1
}

# Ensure /tmp/import exists in container
& $ContainerCmd exec agentteams-manager mkdir -p /tmp/import 2>$null | Out-Null

# ============================================================
# YAML mode: -File
# ============================================================

if ($File) {
    if (-not (Test-Path $File)) {
        Write-Host "[AgentTeams Import ERROR] File not found: $File" -ForegroundColor Red
        exit 1
    }

    $FileName = Split-Path $File -Leaf
    & $ContainerCmd cp $File "agentteams-manager:/tmp/import/$FileName"
    Write-Host "[AgentTeams Import] Copied $FileName -> container:/tmp/import/" -ForegroundColor Cyan

    $agentteamsArgs = @("apply", "-f", "/tmp/import/$FileName")
    if ($Prune) { $agentteamsArgs += "--prune" }
    if ($DryRun) { $agentteamsArgs += "--dry-run" }
    # Accept -Yes for wrapper compatibility, but do not forward it because the
    # container-internal AgentTeams CLI does not support --yes.

    & $ContainerCmd exec agentteams-manager agt @agentteamsArgs
    exit $LASTEXITCODE
}

# ============================================================
# Resource subcommand mode
# ============================================================

switch ($ResourceType) {
    "worker" {
        if (-not $Name) {
            Write-Host "[AgentTeams Import ERROR] -Name is required for worker import" -ForegroundColor Red
            exit 1
        }

        $agentteamsArgs = @("apply", "worker", "--name", $Name)

        # Handle -Zip: download URL if needed, docker cp into container
        if ($Zip) {
            $DownloadedZip = ""
            if ($Zip -match "^https?://") {
                Write-Host "[AgentTeams Import] Downloading $Zip..." -ForegroundColor Cyan
                $DownloadedZip = Join-Path ([System.IO.Path]::GetTempPath()) "agentteams-import-$([System.Guid]::NewGuid().ToString('N').Substring(0,8)).zip"
                try {
                    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
                    Invoke-WebRequest -Uri $Zip -OutFile $DownloadedZip -UseBasicParsing
                    $Zip = $DownloadedZip
                } catch {
                    if ($DownloadedZip -and (Test-Path $DownloadedZip)) { Remove-Item $DownloadedZip -Force }
                    Write-Host "[AgentTeams Import ERROR] Download failed: $_" -ForegroundColor Red
                    exit 1
                }
            }

            if (-not (Test-Path $Zip)) {
                Write-Host "[AgentTeams Import ERROR] ZIP file not found: $Zip" -ForegroundColor Red
                exit 1
            }

            $ZipBaseName = Split-Path $Zip -Leaf
            & $ContainerCmd cp $Zip "agentteams-manager:/tmp/import/$ZipBaseName"
            Write-Host "[AgentTeams Import] Copied $ZipBaseName -> container:/tmp/import/" -ForegroundColor Cyan
            $agentteamsArgs += @("--zip", "/tmp/import/$ZipBaseName")

            # Cleanup downloaded file
            if ($DownloadedZip -and (Test-Path $DownloadedZip)) {
                Remove-Item $DownloadedZip -Force -ErrorAction SilentlyContinue
            }
        }

        # Other params
        if (-not $Zip -and -not $Package) { $Package = $Name }
        if ($Model) { $agentteamsArgs += @("--model", $Model) }
        if ($Package) { $agentteamsArgs += @("--package", $Package) }
        if ($Skills) { $agentteamsArgs += @("--skills", $Skills) }
        if ($McpServers) { $agentteamsArgs += @("--mcp-servers", $McpServers) }
        if ($Runtime) { $agentteamsArgs += @("--runtime", $Runtime) }
        if ($DryRun) { $agentteamsArgs += "--dry-run" }

        & $ContainerCmd exec agentteams-manager agt @agentteamsArgs
        exit $LASTEXITCODE
    }

    { $_ -in @("-h", "--help", "", $null) } {
        Write-Host "Usage:"
        Write-Host "  .\agentteams-import.ps1 worker -Name <name> -Zip <path-or-url>"
        Write-Host "  .\agentteams-import.ps1 worker -Name <name> -Package <nacos://...> [-Model MODEL]"
        Write-Host "  .\agentteams-import.ps1 worker -Name <name>                        # auto-import package <name>"
        Write-Host "  .\agentteams-import.ps1 worker -Name <name> -Model MODEL [-Skills s1,s2] [-McpServers m1,m2]"
        Write-Host "  .\agentteams-import.ps1 -File <resource.yaml> [-Prune] [-DryRun]"
        exit 0
    }

    default {
        Write-Host "[AgentTeams Import ERROR] Unknown resource type: $ResourceType" -ForegroundColor Red
        Write-Host "Supported: worker"
        Write-Host "For YAML mode: .\agentteams-import.ps1 -File <resource.yaml>"
        exit 1
    }
}
