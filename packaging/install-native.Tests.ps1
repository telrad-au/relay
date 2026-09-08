# Disposable Windows CI hosts only: exercises the actual SCM service and ACLs.
$ErrorActionPreference = 'Stop'
if ($env:TELRAD_NATIVE_INSTALL_TEST -ne '1') { throw 'Native installation tests require a disposable host.' }
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('relay-native-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $fixture | Out-Null
try {
    go build -ldflags '-X main.version=0.0.0-ci.1' -o (Join-Path $fixture 'telrad-relay.exe') ./cmd/telrad-relay
    if ($LASTEXITCODE -ne 0) { throw 'Build failed.' }
    Copy-Item packaging/install.ps1, packaging/relay.example.json $fixture
    '{"schemaVersion":1,"channel":"stable","manifestUrl":"https://example.invalid/stable.json","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}' | Set-Content -Encoding Ascii (Join-Path $fixture 'update-trust.json')
    '{"schemaVersion":1,"version":"0.0.0-ci.1"}' | Set-Content -Encoding Ascii (Join-Path $fixture 'installation-manifest.json')
    # Fail after SCM creation to verify a fresh installation removes its new
    # service and executable when later firewall configuration fails.
    $failed = $false
    $failureLog = Join-Path $fixture 'firewall-failure.log'
    try { & (Join-Path $fixture 'install.ps1') -ClinicRemoteAddress 'invalid-review-scope' *> $failureLog } catch { $failed = $true }
    if (-not $failed) { throw 'Invalid firewall scope was accepted.' }
    if ((Get-Content -Raw $failureLog) -notmatch 'Relay firewall configuration failed:') {
        throw "Installation failed before exercising firewall rollback: $(Get-Content -Raw $failureLog)"
    }
    if (Get-Service TelradRelay -ErrorAction SilentlyContinue) { throw 'Failed installation left an SCM service.' }
    $failedBinary = Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'Telrad Relay\telrad.exe'
    if (Test-Path $failedBinary) { throw 'Failed installation left an executable.' }
    & (Join-Path $fixture 'install.ps1')
    $binary = Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'Telrad Relay\telrad.exe'
    $config = Join-Path ([Environment]::GetFolderPath('CommonApplicationData')) 'Telrad\Relay\relay.json'
    for ($attempt=0; $attempt -lt 30; $attempt++) {
        $statusText = & $binary status 2>&1
        if ($LASTEXITCODE -eq 0) { break }
        Start-Sleep -Seconds 1
    }
    if ($LASTEXITCODE -ne 0 -or ($statusText -join "`n") -notmatch 'ingest ready: false') { throw 'Unpaired management did not start.' }
    $service = Get-CimInstance Win32_Service -Filter "Name='TelradRelay'"
    if ($service.StartName -ne 'NT SERVICE\TelradRelay') { throw 'Incorrect service identity.' }
    & $binary run
    if ($LASTEXITCODE -eq 0) { throw 'Administrator was allowed to run the managed daemon.' }
    Set-NetFirewallRule -Name TelradRelay-DICOM -Enabled False
    $before = Get-Content -Raw $config | ConvertFrom-Json
    $before.reportPort = 32576
    $before | ConvertTo-Json -Depth 20 -Compress | Set-Content -Encoding Ascii $config
    & (Join-Path $fixture 'install.ps1')
    if ((Get-Content -Raw $config | ConvertFrom-Json).reportPort -ne 32576) { throw 'Upgrade lost configuration.' }
    if ((Get-NetFirewallRule -Name TelradRelay-DICOM).Enabled -ne 'False') { throw 'Upgrade changed an operator firewall rule.' }
    Stop-Service TelradRelay
    $outsideDirectory = Join-Path $fixture 'outside-directory'
    New-Item -ItemType Directory -Path $outsideDirectory | Out-Null
    $outsideAcl = (Get-Acl $outsideDirectory).Sddl
    $junction = Join-Path (Split-Path $config) 'unmanaged-junction'
    New-Item -ItemType Junction -Path $junction -Target $outsideDirectory | Out-Null
    & (Join-Path $fixture 'install.ps1')
    if ((Get-Acl $outsideDirectory).Sddl -ne $outsideAcl) { throw 'Installer propagated ACLs through an unmanaged junction.' }
    [IO.Directory]::Delete($junction)

    if ((Get-Service TelradRelay).Status -ne 'Stopped') { throw 'Repair restarted a deliberately stopped service.' }
    # A linked state file must fail before any write to its other name.
    $sentinel = Join-Path $fixture 'sentinel'
    'outside sentinel' | Set-Content -Encoding Ascii $sentinel
    Move-Item $config ($config + '.saved')
    New-Item -ItemType HardLink -Path $config -Target $sentinel | Out-Null
    try {
        $failed = $false
        try { & (Join-Path $fixture 'install.ps1') } catch { $failed = $true }
        if (-not $failed) { throw 'Installer accepted a hard-linked state file.' }
        if ((Get-Content -Raw $sentinel).Trim() -ne 'outside sentinel') { throw 'Outside file was changed.' }
    } finally {
        Remove-Item -LiteralPath $config
        Move-Item ($config + '.saved') $config
    }
    $env:TELRAD_NATIVE_NEXT_BINARY = Join-Path $fixture 'telrad-next.exe'
    go build -ldflags '-X main.version=0.0.0-ci.2' -o $env:TELRAD_NATIVE_NEXT_BINARY ./cmd/telrad-relay
    if ($LASTEXITCODE -ne 0) { throw 'Candidate build failed.' }
    $env:TELRAD_NATIVE_LIFECYCLE_TEST = '1'
    go test ./cmd/telrad-relay -run '^TestNative(InstalledLifecycle|SystemRollback)$' -v -count=1 -timeout=5m
    if ($LASTEXITCODE -ne 0) { throw 'Native lifecycle checks failed.' }
    Write-Host 'Native Windows installation and privilege boundary checks passed.'
} finally {
    Stop-Service TelradRelay -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $fixture
}
