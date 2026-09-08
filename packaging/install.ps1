param([string]$ClinicRemoteAddress = "LocalSubnet")
$ErrorActionPreference = "Stop"
$OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run PowerShell as Administrator."
}
# Resolve bundle files from the script location, including piped/testing installs.
$bundle = $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($bundle)) { $bundle = (Get-Location).Path }
$inputData = @{
    config = Get-Content -Raw -LiteralPath (Join-Path $bundle "relay.example.json") | ConvertFrom-Json
    trust = Get-Content -Raw -LiteralPath (Join-Path $bundle "update-trust.json") | ConvertFrom-Json
    installation = Get-Content -Raw -LiteralPath (Join-Path $bundle "installation-manifest.json") | ConvertFrom-Json
    clinicRemoteAddress = $ClinicRemoteAddress
}
$inputData | ConvertTo-Json -Depth 20 -Compress | & (Join-Path $bundle "telrad-relay.exe") install-native
if ($LASTEXITCODE -ne 0) { throw "Relay native installation failed." }
$target = Join-Path ([Environment]::GetFolderPath('ProgramFiles')) "Telrad Relay"
$processPath = [Environment]::GetEnvironmentVariable("Path", "Process")
if (($processPath -split ";") -notcontains $target) {
    $updatedProcessPath = if ([string]::IsNullOrWhiteSpace($processPath)) { $target } else { "$target;$processPath" }
    [Environment]::SetEnvironmentVariable("Path", $updatedProcessPath, "Process")
}
