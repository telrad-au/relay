param([string]$ClinicRemoteAddress = "LocalSubnet")

$ErrorActionPreference = "Stop"
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run PowerShell as Administrator."
}
$serviceName = "TelradRelay"
$serviceIdentity = "NT SERVICE\$serviceName"
$target = Join-Path $env:ProgramFiles "Telrad Relay"
$config = Join-Path $env:ProgramData "Telrad\Relay"
$installedBinary = Join-Path $target "telrad.exe"
$hadInstallation = Test-Path $installedBinary
$hadEnrollment = Test-Path (Join-Path $config "relay-credential.json")
$existing = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
$serviceWasRunning = $null -ne $existing -and $existing.Status -eq "Running"
if ($serviceWasRunning) {
    Stop-Service $serviceName -Force
}
New-Item -ItemType Directory -Force -Path $target, $config | Out-Null
Copy-Item .\telrad-relay.exe $installedBinary -Force
Copy-Item .\update-trust.json (Join-Path $target "update-trust.json") -Force
$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
if (($machinePath -split ";") -notcontains $target) {
    [Environment]::SetEnvironmentVariable("Path", ($machinePath.TrimEnd(";") + ";" + $target), "Machine")
}
if (-not (Test-Path (Join-Path $config "relay.json"))) {
    Copy-Item .\relay.example.json (Join-Path $config "relay.json")
}
$installedSchema = (Get-Content -Raw (Join-Path $config "relay.json") | ConvertFrom-Json).schemaVersion
if ($installedSchema -eq 2) {
    & $installedBinary --config (Join-Path $config "relay.json") migrate-config
    if ($LASTEXITCODE -ne 0) { throw "Relay configuration migration failed." }
} elseif ($installedSchema -ne 3) {
    throw "The installed Relay configuration schema is unsupported."
}
Copy-Item .\installation-manifest.json (Join-Path $config "installation.json") -Force
$binary = '"' + $installedBinary + '" run'
$startMode = if ($hadEnrollment) { "auto" } else { "demand" }
if ($existing) {
    sc.exe config $serviceName binPath= $binary start= $startMode obj= $serviceIdentity password= "" | Out-Null
} else {
    sc.exe create $serviceName binPath= $binary start= $startMode obj= $serviceIdentity password= "" DisplayName= "Telrad Relay" | Out-Null
    sc.exe failure $serviceName reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null
}
sc.exe sidtype $serviceName unrestricted | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Could not enable the relay service SID." }
& icacls.exe $target /inheritance:r | Out-Null
& icacls.exe $target /grant:r "SYSTEM:(OI)(CI)F" "BUILTIN\Administrators:(OI)(CI)F" "${serviceIdentity}:(OI)(CI)RX" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Could not secure $target for the relay service account." }
& icacls.exe $config /inheritance:r | Out-Null
& icacls.exe $config /grant:r "SYSTEM:(OI)(CI)F" "BUILTIN\Administrators:(OI)(CI)F" "${serviceIdentity}:(OI)(CI)M" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Could not secure $config for the relay service account." }
foreach ($rule in @(
    @{ Name = "Telrad Relay DICOM"; Port = 11112 },
    @{ Name = "Telrad Relay HL7"; Port = 2575 }
)) {
    Remove-NetFirewallRule -DisplayName $rule.Name -ErrorAction SilentlyContinue
    New-NetFirewallRule -DisplayName $rule.Name -Direction Inbound -Action Allow -Protocol TCP -LocalPort $rule.Port -RemoteAddress $ClinicRemoteAddress -Profile Domain,Private | Out-Null
}
if (-not [Diagnostics.EventLog]::SourceExists($serviceName)) {
    New-EventLog -LogName Application -Source $serviceName
}
$installedVersion = & $installedBinary version
Write-Host "Telrad Relay $installedVersion installed."
if ($hadEnrollment) {
    Write-Host "Existing authentication preserved."
} elseif ($hadInstallation) {
    Write-Host "Existing configuration preserved."
}
if ($serviceWasRunning) {
    Start-Service $serviceName
    Write-Host "Service restarted successfully."
    Write-Host "Relay is running."
} elseif ($hadEnrollment) {
    Write-Host "The service remains stopped. Run 'telrad' to start it."
} else {
    Write-Host "Run 'telrad' to authenticate this host and start the service."
}
$processPath = [Environment]::GetEnvironmentVariable("Path", "Process")
if (($processPath -split ";") -notcontains $target) {
    $updatedProcessPath = if ([string]::IsNullOrWhiteSpace($processPath)) {
        $target
    } else {
        "$target;$processPath"
    }
    [Environment]::SetEnvironmentVariable("Path", $updatedProcessPath, "Process")
}
