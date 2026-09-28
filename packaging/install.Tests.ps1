# Disposable Windows CI hosts only: installs, upgrades and removes the real
# TelradRelay service with packaging/install.ps1 against locally built
# releases, and exercises the telrad operator commands against it.
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

# Runs telrad and returns its combined output and exit code. Piping $null
# makes standard input a pipe rather than a console, so nothing can confirm.
function Invoke-Telrad([string[]]$arguments) {
    $output = ($null | & $exe @arguments 2>&1) -join "`n"
    [pscustomobject]@{ Output = $output; ExitCode = $LASTEXITCODE }
}

function Assert-Fails([scriptblock]$action, [string]$message) {
    $failed = $false
    try { & $action *> $null } catch { $failed = $true }
    if (-not $failed) { throw $message }
}

try {
    $first = New-Release '0.0.0-ci.1'
    $second = New-Release '0.0.0-ci.2'

    # A shipped installer carries its release tag in place of the empty
    # placeholder, as scripts/build-release.sh writes it; -ReleaseUrl still wins.
    $placeholder = "`$releaseTag = ''"
    $source = Get-Content -LiteralPath $installer
    if (@($source | Where-Object { $_ -ceq $placeholder }).Count -ne 1) { throw 'install.ps1 must contain the empty release tag placeholder once.' }
    $tagged = Join-Path $work 'install.ps1'
    Set-Content -LiteralPath $tagged -Value ($source | ForEach-Object { if ($_ -ceq $placeholder) { "`$releaseTag = 'main-7-gabcdef0'" } else { $_ } })

    # A checksum mismatch installs nothing.
    $tampered = Join-Path $work 'tampered'
    Copy-Item -Recurse $first $tampered
    [IO.File]::AppendAllText((Join-Path $tampered $asset), 'x')
    Assert-Fails { & $installer -ReleaseUrl $tampered -ReportHost 127.0.0.1 } 'Installer accepted a tampered binary.'
    if (Get-Service TelradRelay -ErrorAction SilentlyContinue) { throw 'Rejected installation created the service.' }
    if (Test-Path $exe) { throw 'Rejected installation left an executable.' }

    & $tagged -ReleaseUrl $first -ReportHost 127.0.0.1
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
    # -Version main picks the numerically newest main build from the release
    # listing; -ReleaseUrl still supplies the files.
    $releases = Join-Path $work 'releases.json'
    [IO.File]::WriteAllText($releases, '[{"tag_name":"main-9-g1111111"},{"tag_name":"v2.1.0"},{"tag_name":"main-100-gaaaaaaa"},{"tag_name":"v2.1.0-rc.1"},{"tag_name":"main-10-gbbbbbbb"}]')
    $env:TELRAD_RELAY_RELEASES_API = $releases
    $output = & $installer -Version main -ReleaseUrl $second 6>&1 | Out-String
    if (-not $output.Contains('Telrad Relay main build: main-100-gaaaaaaa')) { throw "Installer chose the wrong main build: $output" }
    Wait-Status 'Telrad Relay 0.0.0-ci.2'
    if ((Get-Content -Raw $config | ConvertFrom-Json).reportPort -ne 32576) { throw 'Upgrade lost configuration.' }
    if ((Get-NetFirewallRule -Name TelradRelay-DICOM).Enabled -ne 'False') { throw 'Upgrade changed an operator firewall rule.' }

    # A main build that is not listed fails before anything changes.
    $refusal = ''
    try { & $installer -Version main-11 -ReleaseUrl $first *> $null } catch { $refusal = "$_" }
    if (-not $refusal.Contains('main build 11 was not found')) { throw "Installer did not refuse a missing main build: $refusal" }
    if ((& $exe version) -ne '0.0.0-ci.2') { throw 'A refused main build changed the installed version.' }
    Remove-Item Env:TELRAD_RELAY_RELEASES_API

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

    # report-receiver shows the receiver and sets it, changing only reportHost
    # and reportPort and restarting the running service.
    Start-Service TelradRelay
    $shown = Invoke-Telrad @('report-receiver')
    if ($shown.ExitCode -ne 0 -or $shown.Output -ne 'report receiver: 127.0.0.1:32576') { throw "report-receiver showed: $($shown.Output)" }
    foreach ($receiver in 'bad host', '127.0.0.1:0', '[127.0.0.1]:2576') {
        if ((Invoke-Telrad @('report-receiver', $receiver)).ExitCode -eq 0) { throw "report-receiver accepted $receiver." }
    }
    $set = Invoke-Telrad @('report-receiver', '127.0.0.1:12577')
    if ($set.ExitCode -ne 0 -or -not $set.Output.Contains('now delivers reports to 127.0.0.1:12577')) { throw "report-receiver failed: $($set.Output)" }
    Wait-Status 'report receiver: 127.0.0.1:12577'
    $settings = Get-Content -Raw $config | ConvertFrom-Json
    if ($settings.schemaVersion -ne 6 -or $settings.reportHost -ne '127.0.0.1' -or $settings.reportPort -ne 12577) { throw 'report-receiver wrote the wrong configuration.' }
    if ((Invoke-Telrad @('report-receiver', '[::1]')).ExitCode -ne 0) { throw 'report-receiver refused an IPv6 address.' }
    Wait-Status 'report receiver: [::1]:12577'

    # pair --yes removes only identity.json. The enrolment URL never resolves,
    # so no link appears and pair fails after its bounded wait.
    Stop-Service TelradRelay
    $ledger = Join-Path $dataDir 'accessions.ledger'
    [IO.File]::AppendAllText($ledger, "ACC-SYNTHETIC-1`n")
    [IO.File]::WriteAllText((Join-Path $dataDir 'identity.json'), 'not an identity')
    if ((Invoke-Telrad @('accept-backlog', '--hours', '1')).ExitCode -ne 0) { throw 'accept-backlog failed.' }
    $refused = Invoke-Telrad @('pair')
    if ($refused.ExitCode -eq 0 -or -not $refused.Output.Contains('rerun with --yes')) { throw "pair without a console: $($refused.Output)" }
    if (-not (Test-Path (Join-Path $dataDir 'identity.json'))) { throw 'Refused pair removed identity.json.' }
    $paired = Invoke-Telrad @('pair', '--yes')
    if ($paired.ExitCode -eq 0 -or -not $paired.Output.Contains('pairing problem: enrolment request failed')) { throw "pair --yes: $($paired.Output)" }
    if (Test-Path (Join-Path $dataDir 'identity.json')) { throw 'pair --yes kept identity.json.' }
    if (-not (Get-Content $ledger).Contains('ACC-SYNTHETIC-1')) { throw 'pair --yes changed the ledger.' }
    if (-not (Test-Path (Join-Path $dataDir 'accept-backlog.json'))) { throw 'pair --yes removed the backlog window.' }
    if ((Get-Service TelradRelay).Status -ne 'Running') { throw 'pair --yes left the service stopped.' }

    # uninstall refuses without a console, then removes the service, firewall
    # rules, PATH entry and program directory but keeps the data directory.
    if ((Invoke-Telrad @('uninstall')).ExitCode -eq 0 -or -not (Test-Path $exe)) { throw 'uninstall ran without confirmation.' }
    $removed = Invoke-Telrad @('uninstall', '--yes')
    if ($removed.ExitCode -ne 0 -or -not $removed.Output.Contains('Telrad Relay was removed.')) { throw "uninstall failed: $($removed.Output)" }
    if (Get-Service TelradRelay -ErrorAction SilentlyContinue) { throw 'uninstall kept the service.' }
    if (Get-NetFirewallRule -Name TelradRelay-DICOM, TelradRelay-HL7 -ErrorAction SilentlyContinue) { throw 'uninstall kept the firewall rules.' }
    if (([Environment]::GetEnvironmentVariable('Path', 'Machine') -split ';') -contains $installDir) { throw 'uninstall kept the PATH entry.' }
    if (Test-Path -LiteralPath $installDir) { throw "uninstall left ${installDir}: $($removed.Output)" }
    if (-not (Get-Content $ledger).Contains('ACC-SYNTHETIC-1') -or -not (Test-Path $config)) { throw 'uninstall removed the data directory.' }

    # A reinstall resumes with the kept configuration; --purge removes it too.
    & $installer -ReleaseUrl $second
    Wait-Status 'report receiver: [::1]:12577'
    $purged = Invoke-Telrad @('uninstall', '--purge', '--yes')
    if ($purged.ExitCode -ne 0) { throw "uninstall --purge failed: $($purged.Output)" }
    if (Test-Path -LiteralPath $installDir) { throw "uninstall --purge left ${installDir}: $($purged.Output)" }
    if (Test-Path $dataDir) { throw 'uninstall --purge kept the data directory.' }

    Write-Host 'Windows installer checks passed.'
} finally {
    Remove-Item Env:TELRAD_RELAY_RELEASES_API -ErrorAction SilentlyContinue
    Stop-Service TelradRelay -ErrorAction SilentlyContinue
    & sc.exe delete TelradRelay | Out-Null
    Remove-NetFirewallRule -Name TelradRelay-DICOM, TelradRelay-HL7 -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $installDir, $dataDir, $work -ErrorAction SilentlyContinue
}
