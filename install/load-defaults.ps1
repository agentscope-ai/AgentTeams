# load-defaults.ps1 — parse install/defaults.env for PowerShell installers.
# Usage: . "$PSScriptRoot/load-defaults.ps1"

function Import-InstallDefaults {
    param(
        [Parameter(Mandatory = $true)]
        [string]$DefaultsFile
    )

    if (-not (Test-Path -LiteralPath $DefaultsFile)) {
        throw "Install defaults file not found: $DefaultsFile"
    }

    foreach ($line in Get-Content -LiteralPath $DefaultsFile) {
        $trimmed = $line.Trim()
        if ($trimmed -eq "" -or $trimmed.StartsWith("#")) {
            continue
        }

        if ($trimmed -match '^\s*:\s*"\$\{([^}]+):=([^}]*)\}"\s*$') {
            $name = $Matches[1]
            $defaultValue = $Matches[2]
            if (-not (Get-Item -Path "Env:$name" -ErrorAction SilentlyContinue)) {
                Set-Item -Path "Env:$name" -Value $defaultValue
            }
            continue
        }

        if ($trimmed -match '^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            $name = $Matches[1]
            $value = $Matches[2]
            Set-Item -Path "Env:$name" -Value $value
        }
    }
}

Import-InstallDefaults -DefaultsFile (Join-Path $PSScriptRoot "defaults.env")
