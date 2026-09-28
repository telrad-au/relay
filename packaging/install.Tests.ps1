# Disposable Windows CI hosts only: installs, upgrades and removes the real
# TelradRelay service with packaging/install.ps1 against locally built releases.
$ErrorActionPreference = 'Stop'
if ($env:TELRAD_NATIVE_INSTALL_TEST -ne '1') { throw 'Installer tests require a disposable host (TELRAD_NATIVE_INSTALL_TEST=1).' }

$root = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $root 'packaging\install.ps1'
$programFiles = if ($env:ProgramW6432) { $env:ProgramW6432 } else { $env:ProgramFiles }
$installDir = Join-Path $programFiles 'Telrad Relay'
$exe = Join-Path $installDir 'telrad.exe'
$dataDir = Join-Path $env:ProgramData 'Telrad\Relay'
$config = Join-Path $dataDir 'relay.json'
$asset = 'telrad-relay-windows-amd64.exe'
$work = Join-Path ([IO.Path]::GetTempPath()) ('relay-install-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null

# The enrolment endpoint never resolves, so CI never contacts Telrad.
function New-Release([string]$version) {
    $directory = Join-Path $work $version
    New-Item -ItemType Directory -Path $directory | Out-Null
    Push-Location $root
    try {
        go build -trimpath -ldflags "-s -w -X main.version=$version -X main.defaultEnrolmentURL=https://enrolment.invalid/api/relay/enrolments" -o (Join-Path $directory $asset) ./cmd/telrad-relay
        if ($LASTEXITCODE -ne 0) { throw "Build of $version failed." }
    } finally {
        Pop-Location
    }
    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $directory $asset)).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $directory 'SHA256SUMS'), "$hash  $asset`n")
    $directory
}

function Wait-Status([string]$expected) {
    $text = ''
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        $text = (& $exe status 2>&1) -join "`n"
        if ($LASTEXITCODE -eq 0 -and $text.Contains($expected)) { return }
        Start-Sleep -Seconds 1
    }
    throw "telrad status did not report '${expected}': $text"
}

function Assert-Fails([scriptblock]$action, [string]$message) {
    $failed = $false
    try { & $action *> $null } catch { $failed = $true }
    if (-not $failed) { throw $message }
}

try {
    $first = New-Release '0.0.0-ci.1'
    $second = New-Release '0.0.0-ci.2'

    # A checksum mismatch installs nothing.
    $tampered = Join-Path $work 'tampered'
    Copy-Item -Recurse $first $tampered
    [IO.File]::AppendAllText((Join-Path $tampered $asset), 'x')
    Assert-Fails { & $installer -ReleaseUrl $tampered -ReportHost 127.0.0.1 } 'Installer accepted a tampered binary.'
    if (Get-Service TelradRelay -ErrorAction SilentlyContinue) { throw 'Rejected installation created the service.' }
    if (Test-Path $exe) { throw 'Rejected installation left an executable.' }

    & $installer -ReleaseUrl $first -ReportHost 127.0.0.1
    Wait-Status 'state: pairing'
    if ((& $exe version) -ne '0.0.0-ci.1') { throw 'Installed version is wrong.' }
    $service = Get-CimInstance Win32_Service -Filter "Name='TelradRelay'"
    if ($service.StartName -ne 'NT SERVICE\TelradRelay') { throw "Service runs as $($service.StartName)." }
    if ($service.StartMode -ne 'Auto') { throw 'Service does not start automatically.' }
    $acl = Get-Acl -LiteralPath $dataDir
    if (-not $acl.AreAccessRulesProtected) { throw 'Data directory inherits permissions.' }
    $principals = @($acl.Access | ForEach-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value } | Sort-Object -Unique)
    $serviceSid = (New-Object Security.Principal.NTAccount('NT SERVICE\TelradRelay')).Translate([Security.Principal.SecurityIdentifier]).Value
    $allowed = @('S-1-5-18', 'S-1-5-32-544', $serviceSid)
    if (@($principals | Where-Object { $allowed -notcontains $_ }).Count -ne 0 -or $principals.Count -ne 3) {
        throw "Data directory grants unexpected principals: $($principals -join ', ')"
    }
    if ((Get-Content -Raw $config | ConvertFrom-Json).reportHost -ne '127.0.0.1') { throw 'relay.json does not carry the report host.' }
    foreach ($name in 'TelradRelay-DICOM', 'TelradRelay-HL7') {
        $filter = Get-NetFirewallRule -Name $name | Get-NetFirewallAddressFilter
        if ($filter.RemoteAddress -ne 'LocalSubnet') { throw "$name is not scoped to LocalSubnet." }
    }
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (($machinePath -split ';') -notcontains $installDir) { throw 'Install directory is not on the machine PATH.' }

    # Upgrade keeps configuration and operator firewall changes, and restarts
    # the running service on the new version.
    $settings = Get-Content -Raw $config | ConvertFrom-Json
    $settings.reportPort = 32576
    [IO.File]::WriteAllText($config, ($settings | ConvertTo-Json), (New-Object Text.UTF8Encoding($false)))
    Set-NetFirewallRule -Name TelradRelay-DICOM -Enabled False
    & $installer -ReleaseUrl $second
    Wait-Status 'Telrad Relay 0.0.0-ci.2'
    if ((Get-Content -Raw $config | ConvertFrom-Json).reportPort -ne 32576) { throw 'Upgrade lost configuration.' }
    if ((Get-NetFirewallRule -Name TelradRelay-DICOM).Enabled -ne 'False') { throw 'Upgrade changed an operator firewall rule.' }

    # A deliberately stopped service stays stopped.
    Stop-Service TelradRelay
    & $installer -ReleaseUrl $second
    if ((Get-Service TelradRelay).Status -ne 'Stopped') { throw 'Installer started a deliberately stopped service.' }

    # A link in the data directory stops the installer before any ACL change.
    $outside = Join-Path $work 'outside'
    New-Item -ItemType Directory -Path $outside | Out-Null
    $outsideAcl = (Get-Acl $outside).Sddl
    $junction = Join-Path $dataDir 'junction'
    New-Item -ItemType Junction -Path $junction -Target $outside | Out-Null
    Assert-Fails { & $installer -ReleaseUrl $second } 'Installer accepted a link in the data directory.'
    if ((Get-Acl $outside).Sddl -ne $outsideAcl) { throw 'Installer changed permissions through a junction.' }
    [IO.Directory]::Delete($junction)

    Write-Host 'Windows installer checks passed.'
} finally {
    Stop-Service TelradRelay -ErrorAction SilentlyContinue
    & sc.exe delete TelradRelay | Out-Null
    Remove-NetFirewallRule -Name TelradRelay-DICOM, TelradRelay-HL7 -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $installDir, $dataDir, $work -ErrorAction SilentlyContinue
}
