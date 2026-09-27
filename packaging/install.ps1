# Telrad Relay installer for Windows. Run in an elevated PowerShell:
#
#   irm https://github.com/telrad-au/relay/releases/latest/download/install.ps1 | iex
#
# Install or upgrade to a specific version with -Version (or
# $env:TELRAD_RELAY_VERSION). Rerunning the installer is the only upgrade path:
#
#   & ([scriptblock]::Create((irm https://github.com/telrad-au/relay/releases/latest/download/install.ps1))) -Version 1.2.3
#
# The installer downloads telrad-relay-windows-amd64.exe, verifies it against
# the release's SHA256SUMS, installs it as %ProgramFiles%\Telrad Relay\telrad.exe
# (added to the machine PATH), and runs it as the TelradRelay service under the
# virtual account NT SERVICE\TelradRelay. %ProgramData%\Telrad\Relay holds
# relay.json, the key, the certificate and the ledger and is accessible only
# to SYSTEM, Administrators and the service, so run telrad from an elevated
# prompt. The service starts on a first install and restarts if it was
# running; a service that an operator stopped stays stopped.
#
# Inbound firewall rules TelradRelay-DICOM (TCP 11112) and TelradRelay-HL7
# (TCP 2575) allow telrad.exe from -ClinicRemoteAddress (default LocalSubnet;
# any New-NetFirewallRule -RemoteAddress value). Existing rules are left as the
# operator configured them unless -ClinicRemoteAddress is passed again.
#
# relay.json is written only when absent and never changed afterwards. Set the
# clinic report receiver with -ReportHost (or $env:TELRAD_RELAY_REPORT_HOST) on
# the first install. Without it the installer writes report-receiver.invalid,
# a name that never resolves: pairing and order forwarding work and every
# report is answered AE, so Telrad keeps it and retries until the operator sets
# reportHost and runs telrad restart.
#
# -ReleaseUrl (or $env:TELRAD_RELAY_RELEASE_URL) replaces the GitHub release
# directory with another https URL or a local directory, for installer tests.
#
# Remove Relay with:
#   Stop-Service TelradRelay; sc.exe delete TelradRelay
#   Remove-NetFirewallRule -Name TelradRelay-DICOM, TelradRelay-HL7
#   Remove-Item -Recurse "$env:ProgramFiles\Telrad Relay", "$env:ProgramData\Telrad\Relay"
#   and remove "$env:ProgramFiles\Telrad Relay" from the machine PATH.
param(
    [string]$Version = $env:TELRAD_RELAY_VERSION,
    [string]$ReportHost = $env:TELRAD_RELAY_REPORT_HOST,
    [string[]]$ClinicRemoteAddress = @('LocalSubnet'),
    [string]$ReleaseUrl = $env:TELRAD_RELAY_RELEASE_URL
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Fail([string]$message) { throw "telrad-relay install: $message" }

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Fail 'run PowerShell as Administrator' }
if (-not [Environment]::Is64BitOperatingSystem) { Fail '64-bit Windows is required' }

$serviceName = 'TelradRelay'
$serviceAccount = "NT SERVICE\$serviceName"
$programFiles = if ($env:ProgramW6432) { $env:ProgramW6432 } else { $env:ProgramFiles }
$installDir = Join-Path $programFiles 'Telrad Relay'
$exe = Join-Path $installDir 'telrad.exe'
$staged = Join-Path $installDir 'telrad.new.exe'
$companyDir = Join-Path $env:ProgramData 'Telrad'
$dataDir = Join-Path $companyDir 'Relay'
$config = Join-Path $dataDir 'relay.json'
$asset = 'telrad-relay-windows-amd64.exe'
$placeholder = 'report-receiver.invalid'
$system = New-Object Security.Principal.SecurityIdentifier('S-1-5-18')
$administrators = New-Object Security.Principal.SecurityIdentifier('S-1-5-32-544')
$users = New-Object Security.Principal.SecurityIdentifier('S-1-5-32-545')

$Version = "$Version".Trim().TrimStart('v')
if ($Version) {
    if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') { Fail 'version must look like 1.2.3 or 1.2.3-rc.1' }
    $defaultUrl = "https://github.com/telrad-au/relay/releases/download/v$Version"
} else {
    $defaultUrl = 'https://github.com/telrad-au/relay/releases/latest/download'
}
if (-not $ReleaseUrl) { $ReleaseUrl = $defaultUrl }
if (-not $ReportHost) { $ReportHost = $placeholder }
if ($ReportHost -notmatch '^[A-Za-z0-9.:_-]+$') { Fail 'ReportHost must be a hostname or IP address' }

function Get-ReleaseFile([string]$name, [string]$destination) {
    if ($ReleaseUrl -match '^https://') {
        Invoke-WebRequest -UseBasicParsing -Uri "$ReleaseUrl/$name" -OutFile $destination
    } elseif (Test-Path -LiteralPath $ReleaseUrl -PathType Container) {
        Copy-Item -LiteralPath (Join-Path $ReleaseUrl $name) -Destination $destination
    } else {
        Fail 'the release URL must use https or name a local directory'
    }
}

# Replaces the directory's permissions with an Administrators-owned, protected
# ACL. Links are refused so an ACL change never reaches another location.
function Set-PrivateDirectory([string]$path, [hashtable[]]$grants) {
    if (Test-Path -LiteralPath $path) {
        $item = Get-Item -LiteralPath $path -Force
        if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { Fail "$path must be a plain directory" }
    } else {
        New-Item -ItemType Directory -Path $path | Out-Null
    }
    $acl = New-Object Security.AccessControl.DirectorySecurity
    $acl.SetOwner($administrators)
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($grant in $grants) {
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($grant.Sid, $grant.Rights, 'ContainerInherit, ObjectInherit', 'None', 'Allow')))
    }
    Set-Acl -LiteralPath $path -AclObject $acl
}

$work = Join-Path ([IO.Path]::GetTempPath()) ('telrad-relay-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null
try {
    Write-Host "Downloading Telrad Relay from $ReleaseUrl"
    $sums = Join-Path $work 'SHA256SUMS'
    $download = Join-Path $work $asset
    Get-ReleaseFile 'SHA256SUMS' $sums
    Get-ReleaseFile $asset $download
    $pattern = '^([0-9a-f]{64})  ' + [regex]::Escape($asset) + '$'
    $expected = @(foreach ($line in Get-Content -LiteralPath $sums) { if ($line -match $pattern) { $Matches[1] } })
    if ($expected.Count -ne 1) { Fail "SHA256SUMS does not list $asset" }
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $download).Hash.ToLowerInvariant() -ne $expected[0]) {
        Fail 'checksum verification failed; nothing was installed'
    }

    # Stage and run the new binary before touching the installed service.
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Copy-Item -LiteralPath $download -Destination $staged -Force
    $installedVersion = & $staged version
    if ($LASTEXITCODE -ne 0) {
        Remove-Item -LiteralPath $staged -Force
        Fail 'the downloaded binary does not run on this host'
    }

    $service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    $fresh = $null -eq $service
    $running = (-not $fresh) -and $service.Status -ne 'Stopped'
    # Windows cannot replace a running executable, and the ACL step below must
    # not race the service creating files in its data directory.
    if ($running) { Stop-Service -Name $serviceName }
    Move-Item -LiteralPath $staged -Destination $exe -Force

    if ($fresh) {
        New-Service -Name $serviceName -BinaryPathName "`"$exe`" run" -DisplayName 'Telrad Relay' `
            -Description 'Relays clinic DICOM and HL7 to Telrad over TLS.' -StartupType Automatic | Out-Null
    }
    & sc.exe config $serviceName obj= $serviceAccount start= auto | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "could not set the $serviceName service account" }
    # Restart after failures, including a clean exit with an error code.
    & sc.exe failure $serviceName reset= 86400 actions= restart/5000/restart/5000/restart/60000 | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "could not set the $serviceName recovery actions" }
    & sc.exe failureflag $serviceName 1 | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "could not set the $serviceName failure flag" }

    $eventSource = "HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\$serviceName"
    if (-not (Test-Path $eventSource)) {
        New-Item -Path $eventSource -Force | Out-Null
        New-ItemProperty -Path $eventSource -Name EventMessageFile -PropertyType ExpandString -Value '%SystemRoot%\System32\EventCreate.exe' | Out-Null
        New-ItemProperty -Path $eventSource -Name TypesSupported -PropertyType DWord -Value 7 | Out-Null
    }

    $serviceSid = (New-Object Security.Principal.NTAccount($serviceAccount)).Translate([Security.Principal.SecurityIdentifier])
    Set-PrivateDirectory $companyDir @(
        @{ Sid = $system; Rights = 'FullControl' },
        @{ Sid = $administrators; Rights = 'FullControl' },
        @{ Sid = $users; Rights = 'ReadAndExecute' })
    if (Test-Path -LiteralPath $dataDir) {
        $links = @(Get-ChildItem -LiteralPath $dataDir -Recurse -Force -Attributes ReparsePoint -ErrorAction SilentlyContinue)
        if ($links.Count -gt 0) { Fail "$dataDir contains links; remove them and rerun the installer" }
    }
    Set-PrivateDirectory $dataDir @(
        @{ Sid = $system; Rights = 'FullControl' },
        @{ Sid = $administrators; Rights = 'FullControl' },
        @{ Sid = $serviceSid; Rights = 'Modify' })

    $wroteConfig = $false
    if (-not (Test-Path -LiteralPath $config)) {
        # Only reportHost is required; every other field takes its default.
        $json = "{`n  `"schemaVersion`": 6,`n  `"reportHost`": `"$ReportHost`",`n  `"reportPort`": 2576`n}`n"
        [IO.File]::WriteAllText($config, $json, (New-Object Text.UTF8Encoding($false)))
        $wroteConfig = $true
    }

    # Keep Path as REG_EXPAND_SZ, then broadcast the change by setting and
    # clearing a throwaway machine variable so new consoles find telrad.
    $environmentKey = [Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SYSTEM\CurrentControlSet\Control\Session Manager\Environment', $true)
    try {
        $machinePath = [string]$environmentKey.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
        if (($machinePath -split ';') -notcontains $installDir) {
            $environmentKey.SetValue('Path', ($machinePath.TrimEnd(';') + ';' + $installDir), 'ExpandString')
            [Environment]::SetEnvironmentVariable('TELRAD_RELAY_PATH_UPDATED', '1', 'Machine')
            [Environment]::SetEnvironmentVariable('TELRAD_RELAY_PATH_UPDATED', $null, 'Machine')
        }
    } finally {
        $environmentKey.Close()
    }
    if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$env:Path;$installDir" }

    foreach ($rule in @(
            @{ Name = 'TelradRelay-DICOM'; DisplayName = 'Telrad Relay DICOM'; Port = 11112 },
            @{ Name = 'TelradRelay-HL7'; DisplayName = 'Telrad Relay HL7'; Port = 2575 })) {
        if (-not (Get-NetFirewallRule -Name $rule.Name -ErrorAction SilentlyContinue)) {
            New-NetFirewallRule -Name $rule.Name -DisplayName $rule.DisplayName -Direction Inbound -Action Allow `
                -Protocol TCP -LocalPort $rule.Port -RemoteAddress $ClinicRemoteAddress -Program $exe | Out-Null
        } elseif ($PSBoundParameters.ContainsKey('ClinicRemoteAddress')) {
            Set-NetFirewallRule -Name $rule.Name -RemoteAddress $ClinicRemoteAddress
        }
    }

    if ($fresh -or $running) {
        Start-Service -Name $serviceName
    } else {
        Write-Host "The $serviceName service was stopped and has been left stopped; start it with: telrad start"
    }
    Write-Host "Telrad Relay $installedVersion installed."
    if ($wroteConfig -and $ReportHost -eq $placeholder) {
        Write-Host "Set reportHost in $config to the clinic report receiver, then run: telrad restart"
    }
    Write-Host 'Run telrad in an elevated prompt to see the pairing link.'
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
