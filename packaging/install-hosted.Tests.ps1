$ErrorActionPreference = "Stop"

function Invoke-ExpectedFailure {
    param(
        [Parameter(Mandatory = $true)][scriptblock]$Action,
        [Parameter(Mandatory = $true)][string]$Description
    )

    try {
        & $Action
    } catch {
        return
    }
    throw "Expected verification failure: $Description"
}

$temporary = Join-Path ([IO.Path]::GetTempPath()) ("telrad-installer-test-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $temporary | Out-Null
$certificates = [Collections.Generic.List[object]]::new()
$global:TelradTestSignerThumbprints = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
function global:Get-AuthenticodeSignature {
    param([Parameter(Mandatory = $true)][string]$LiteralPath)

    $signature = Microsoft.PowerShell.Security\Get-AuthenticodeSignature -LiteralPath $LiteralPath
    if (
        $signature.Status -eq [System.Management.Automation.SignatureStatus]::UnknownError -and
        $null -ne $signature.SignerCertificate -and
        $global:TelradTestSignerThumbprints.Contains($signature.SignerCertificate.Thumbprint)
    ) {
        return [PSCustomObject]@{
            Status = [System.Management.Automation.SignatureStatus]::Valid
            SignerCertificate = $signature.SignerCertificate
        }
    }
    return $signature
}
try {
    $template = [IO.File]::ReadAllText((Join-Path $PSScriptRoot "install-hosted.ps1.template"))
    $artifactSource = Join-Path $temporary "fixture.exe"
    & go build -o $artifactSource ./cmd/telrad-relay
    if ($LASTEXITCODE -ne 0) {
        throw "Could not build the unsigned Windows test fixture."
    }

    function New-TestSigner {
        param([Parameter(Mandatory = $true)][string]$Name)

        $certificate = New-SelfSignedCertificate `
            -Type CodeSigningCert `
            -Subject "CN=$Name" `
            -CertStoreLocation "Cert:\CurrentUser\My"
        $certificates.Add($certificate)
        [void]$global:TelradTestSignerThumbprints.Add($certificate.Thumbprint)
        return $certificate
    }

    $trustedCertificate = New-TestSigner -Name "Telrad Test Publisher"
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        $trustedPin = ([BitConverter]::ToString($sha256.ComputeHash($trustedCertificate.RawData))).Replace("-", "").ToLowerInvariant()
    } finally {
        $sha256.Dispose()
    }

    $installer = $template.Replace("@@ARTIFACT_BASE_URL@@", "https://example.invalid/releases/v1.2.3")
    $installer = $installer.Replace("@@UPDATE_MANIFEST_URL@@", "https://updates.example.invalid/relay/stable.json")
    $installer = $installer.Replace("@@ENROLLMENT_URL@@", "https://example.invalid/v1/relay/pairing-enrollments")
    $installer = $installer.Replace("@@UPDATE_PUBLIC_KEY@@", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
    $installer = $installer.Replace("@@WINDOWS_SIGNER_CERTIFICATE_SHA256@@", $trustedPin)
    $installerPath = Join-Path $temporary "install.ps1"
    [IO.File]::WriteAllText($installerPath, $installer)

    $validArtifact = Join-Path $temporary "valid.exe"
    Copy-Item $artifactSource $validArtifact
    $signature = Set-AuthenticodeSignature -FilePath $validArtifact -Certificate $trustedCertificate -HashAlgorithm SHA256
    if ($null -eq $signature.SignerCertificate -or $signature.Status -in @(
        [System.Management.Automation.SignatureStatus]::HashMismatch,
        [System.Management.Automation.SignatureStatus]::NotSigned
    )) {
        throw "Could not create a valid Authenticode test fixture: $($signature.Status)"
    }
    $verifiedSignerPinPath = Join-Path $temporary "verified-signer-pin.txt"
    & (Join-Path $PSScriptRoot "../scripts/verify-windows-signature.ps1") `
        -Path $validArtifact `
        -SignerPinOutputPath $verifiedSignerPinPath `
        -AllowMissingTimestamp
    if ([IO.File]::ReadAllText($verifiedSignerPinPath).Trim() -ne $trustedPin) {
        throw "Windows signature verifier returned the wrong signer certificate pin."
    }
    Invoke-ExpectedFailure -Description "missing RFC 3161 timestamp" -Action {
        & (Join-Path $PSScriptRoot "../scripts/verify-windows-signature.ps1") `
            -Path $validArtifact `
            -SignerPinOutputPath $verifiedSignerPinPath
    }
    $validDigest = (Get-FileHash -Algorithm SHA256 $validArtifact).Hash.ToLowerInvariant()
    $validDigestPath = Join-Path $temporary "valid.sha256"
    [IO.File]::WriteAllText($validDigestPath, $validDigest)
    & $installerPath -VerifyArtifactPath $validArtifact -VerifyDigestPath $validDigestPath

    # Execute the rendered installer from memory, as `irm ... | iex` does, and
    # retain its helper in this test scope so current-session PATH behavior can
    # be exercised without performing a machine installation.
    . ([scriptblock]::Create($installer)) -VerifyArtifactPath $validArtifact -VerifyDigestPath $validDigestPath
    $originalProcessPath = [Environment]::GetEnvironmentVariable("Path", "Process")
    $testInstallPath = Join-Path $temporary "Telrad Relay"
    try {
        [Environment]::SetEnvironmentVariable("Path", "C:\Windows\System32", "Process")
        Add-TelradCurrentProcessPath -Path $testInstallPath
        Add-TelradCurrentProcessPath -Path $testInstallPath
        $expectedProcessPath = "$testInstallPath;C:\Windows\System32"
        if ([Environment]::GetEnvironmentVariable("Path", "Process") -cne $expectedProcessPath) {
            throw "Piped Windows installer did not activate its command exactly once in the current process PATH."
        }
    } finally {
        [Environment]::SetEnvironmentVariable("Path", $originalProcessPath, "Process")
    }

    $tamperedArtifact = Join-Path $temporary "tampered.exe"
    Copy-Item $validArtifact $tamperedArtifact
    [IO.File]::AppendAllText($tamperedArtifact, "tampered")
    $tamperedDigestPath = Join-Path $temporary "tampered.sha256"
    [IO.File]::WriteAllText($tamperedDigestPath, (Get-FileHash -Algorithm SHA256 $tamperedArtifact).Hash.ToLowerInvariant())
    Invoke-ExpectedFailure -Description "tampered Authenticode signature" -Action {
        & $installerPath -VerifyArtifactPath $tamperedArtifact -VerifyDigestPath $tamperedDigestPath
    }

    $unsignedArtifact = Join-Path $temporary "unsigned.exe"
    Copy-Item $artifactSource $unsignedArtifact
    $unsignedDigestPath = Join-Path $temporary "unsigned.sha256"
    [IO.File]::WriteAllText($unsignedDigestPath, (Get-FileHash -Algorithm SHA256 $unsignedArtifact).Hash.ToLowerInvariant())
    Invoke-ExpectedFailure -Description "unsigned production artifact" -Action {
        & (Join-Path $PSScriptRoot "../scripts/verify-windows-signature.ps1") `
            -Path $unsignedArtifact `
            -SignerPinOutputPath $verifiedSignerPinPath `
            -AllowMissingTimestamp
    }
    Invoke-ExpectedFailure -Description "missing Authenticode signature" -Action {
        & $installerPath -VerifyArtifactPath $unsignedArtifact -VerifyDigestPath $unsignedDigestPath
    }

    $wrongCertificate = New-TestSigner -Name "Wrong Test Publisher"
    $wrongArtifact = Join-Path $temporary "wrong-signer.exe"
    Copy-Item $artifactSource $wrongArtifact
    $signature = Set-AuthenticodeSignature -FilePath $wrongArtifact -Certificate $wrongCertificate -HashAlgorithm SHA256
    if ($null -eq $signature.SignerCertificate -or $signature.Status -in @(
        [System.Management.Automation.SignatureStatus]::HashMismatch,
        [System.Management.Automation.SignatureStatus]::NotSigned
    )) {
        throw "Could not create the wrong-signer Authenticode fixture: $($signature.Status)"
    }
    $wrongDigestPath = Join-Path $temporary "wrong-signer.sha256"
    [IO.File]::WriteAllText($wrongDigestPath, (Get-FileHash -Algorithm SHA256 $wrongArtifact).Hash.ToLowerInvariant())
    Invoke-ExpectedFailure -Description "untrusted publisher certificate" -Action {
        & $installerPath -VerifyArtifactPath $wrongArtifact -VerifyDigestPath $wrongDigestPath
    }

    $malformedDigestPath = Join-Path $temporary "malformed.sha256"
    [IO.File]::WriteAllText($malformedDigestPath, "$validDigest`n")
    Invoke-ExpectedFailure -Description "malformed checksum metadata" -Action {
        & $installerPath -VerifyArtifactPath $validArtifact -VerifyDigestPath $malformedDigestPath
    }
} finally {
    foreach ($certificate in $certificates) {
        Remove-Item -LiteralPath ("Cert:\CurrentUser\My\" + $certificate.Thumbprint) -Force -ErrorAction SilentlyContinue
    }
    Remove-Item Function:\Get-AuthenticodeSignature -Force -ErrorAction SilentlyContinue
    Remove-Item Function:\Add-TelradCurrentProcessPath -Force -ErrorAction SilentlyContinue
    Remove-Variable TelradTestSignerThumbprints -Scope Global -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
}
