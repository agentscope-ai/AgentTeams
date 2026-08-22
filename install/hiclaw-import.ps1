# hiclaw-import.ps1 - Import Worker/Team/Human resources into HiClaw
#
# Thin shell that delegates to the `hiclaw` CLI inside the Manager container.
# Supports ZIP packages, remote packages (nacos://, http://), and YAML files.
#
# Usage:
#   .\hiclaw-import.ps1 worker -Name <name> -Zip <path-or-url>
#   .\hiclaw-import.ps1 worker -Name <name> -Package <nacos://...> [-Model MODEL]
#   .\hiclaw-import.ps1 worker -Name <name> -Model MODEL [-Skills s1,s2] [-McpServers m1,m2]
#   .\hiclaw-import.ps1 -File <resource.yaml> [-Prune] [-DryRun]

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
    Write-Host "[HiClaw Import ERROR] Neither docker nor podman found." -ForegroundColor Red
    Write-Host ""
    Write-Host "Docker is required to run HiClaw. Install Docker Desktop first, then install HiClaw:" -ForegroundColor Yellow
    Write-Host "  Set-ExecutionPolicy Bypass -Scope Process -Force; `$wc=New-Object Net.WebClient; `$wc.Encoding=[Text.Encoding]::UTF8; iex `$wc.DownloadString('https://higress.ai/hiclaw/install.ps1')"
    exit 1
}

# Verify Manager container
$mgrRunning = & $ContainerCmd ps --filter "name=hiclaw-manager" --format "{{.Names}}" 2>$null
if ($mgrRunning -notmatch "hiclaw-manager") {
    Write-Host "[HiClaw Import ERROR] hiclaw-manager container is not running." -ForegroundColor Red
    Write-Host ""
    # Check if the container exists but is stopped
    $mgrExists = & $ContainerCmd ps -a --filter "name=hiclaw-manager" --format "{{.Names}}" 2>$null
    if ($mgrExists -match "hiclaw-manager") {
        Write-Host "The hiclaw-manager container exists but is stopped. Start it with:" -ForegroundColor Yellow
        Write-Host "  $ContainerCmd start hiclaw-manager"
    } else {
        Write-Host "HiClaw does not appear to be installed. Install it first:" -ForegroundColor Yellow
        Write-Host "  Set-ExecutionPolicy Bypass -Scope Process -Force; `$wc=New-Object Net.WebClient; `$wc.Encoding=[Text.Encoding]::UTF8; iex `$wc.DownloadString('https://higress.ai/hiclaw/install.ps1')"
    }
    exit 1
}

# Ensure /tmp/import exists in container
& $ContainerCmd exec hiclaw-manager mkdir -p /tmp/import 2>$null | Out-Null

# ============================================================
# YAML mode: -File
# ============================================================

if ($File) {
    if (-not (Test-Path $File)) {
        Write-Host "[HiClaw Import ERROR] File not found: $File" -ForegroundColor Red
        exit 1
    }

    $FileName = Split-Path $File -Leaf
    & $ContainerCmd cp $File "hiclaw-manager:/tmp/import/$FileName"
    Write-Host "[HiClaw Import] Copied $FileName -> container:/tmp/import/" -ForegroundColor Cyan

    $hiclawArgs = @("apply", "-f", "/tmp/import/$FileName")
    if ($Prune) { $hiclawArgs += "--prune" }
    if ($DryRun) { $hiclawArgs += "--dry-run" }
    # Accept -Yes for wrapper compatibility, but do not forward it because the
    # container-internal hiclaw CLI does not support --yes.

    & $ContainerCmd exec hiclaw-manager hiclaw @hiclawArgs
    exit $LASTEXITCODE
}

# ============================================================
# Resource subcommand mode
# ============================================================

switch ($ResourceType) {
    "worker" {
        if (-not $Name) {
            Write-Host "[HiClaw Import ERROR] -Name is required for worker import" -ForegroundColor Red
            exit 1
        }

        $hiclawArgs = @("apply", "worker", "--name", $Name)

        # Handle -Zip: download URL if needed, docker cp into container
        if ($Zip) {
            $DownloadedZip = ""
            if ($Zip -match "^https?://") {
                Write-Host "[HiClaw Import] Downloading $Zip..." -ForegroundColor Cyan
                $DownloadedZip = Join-Path ([System.IO.Path]::GetTempPath()) "hiclaw-import-$([System.Guid]::NewGuid().ToString('N').Substring(0,8)).zip"
                try {
                    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
                    Invoke-WebRequest -Uri $Zip -OutFile $DownloadedZip -UseBasicParsing
                    $Zip = $DownloadedZip
                } catch {
                    if ($DownloadedZip -and (Test-Path $DownloadedZip)) { Remove-Item $DownloadedZip -Force }
                    Write-Host "[HiClaw Import ERROR] Download failed: $_" -ForegroundColor Red
                    exit 1
                }
            }

            if (-not (Test-Path $Zip)) {
                Write-Host "[HiClaw Import ERROR] ZIP file not found: $Zip" -ForegroundColor Red
                exit 1
            }

            $ZipBaseName = Split-Path $Zip -Leaf
            & $ContainerCmd cp $Zip "hiclaw-manager:/tmp/import/$ZipBaseName"
            Write-Host "[HiClaw Import] Copied $ZipBaseName -> container:/tmp/import/" -ForegroundColor Cyan
            $hiclawArgs += @("--zip", "/tmp/import/$ZipBaseName")

            # Cleanup downloaded file
            if ($DownloadedZip -and (Test-Path $DownloadedZip)) {
                Remove-Item $DownloadedZip -Force -ErrorAction SilentlyContinue
            }
        }

        # Other params
        if ($Model) { $hiclawArgs += @("--model", $Model) }
        if ($Package) { $hiclawArgs += @("--package", $Package) }
        if ($Skills) { $hiclawArgs += @("--skills", $Skills) }
        if ($McpServers) { $hiclawArgs += @("--mcp-servers", $McpServers) }
        if ($Runtime) { $hiclawArgs += @("--runtime", $Runtime) }
        if ($DryRun) { $hiclawArgs += "--dry-run" }

        & $ContainerCmd exec hiclaw-manager hiclaw @hiclawArgs
        exit $LASTEXITCODE
    }

    { $_ -in @("-h", "--help", "", $null) } {
        Write-Host "Usage:"
        Write-Host "  .\hiclaw-import.ps1 worker -Name <name> -Zip <path-or-url>"
        Write-Host "  .\hiclaw-import.ps1 worker -Name <name> -Package <nacos://...> [-Model MODEL]"
        Write-Host "  .\hiclaw-import.ps1 worker -Name <name> -Model MODEL [-Skills s1,s2] [-McpServers m1,m2]"
        Write-Host "  .\hiclaw-import.ps1 -File <resource.yaml> [-Prune] [-DryRun]"
        exit 0
    }

    default {
        Write-Host "[HiClaw Import ERROR] Unknown resource type: $ResourceType" -ForegroundColor Red
        Write-Host "Supported: worker"
        Write-Host "For YAML mode: .\hiclaw-import.ps1 -File <resource.yaml>"
        exit 1
    }
}
