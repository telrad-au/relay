param(
    [Parameter(Mandatory = $true)][string]$Path,
    [Parameter(Mandatory = $true)][string]$SignerPinOutputPath,
    [switch]$AllowMissingTimestamp
)

$ErrorActionPreference = "Stop"

$signature = Get-AuthenticodeSignature -LiteralPath $Path
if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
    throw "Relay Authenticode signature verification failed: $($signature.Status)."
}
if ($null -eq $signature.SignerCertificate) {
    throw "Relay Authenticode signature has no signer certificate."
}
if (-not $AllowMissingTimestamp -and $null -eq $signature.TimeStamperCertificate) {
    throw "Relay Authenticode signature has no RFC 3161 timestamp."
}

$sha256 = [Security.Cryptography.SHA256]::Create()
try {
    $signerPin = ([BitConverter]::ToString($sha256.ComputeHash($signature.SignerCertificate.RawData))).Replace("-", "").ToLowerInvariant()
} finally {
    $sha256.Dispose()
}
if ($signerPin -notmatch '^[0-9a-f]{64}$') {
    throw "Relay Authenticode signer certificate pin is not a canonical SHA-256 digest."
}

$parent = Split-Path -Parent $SignerPinOutputPath
if (-not [string]::IsNullOrEmpty($parent)) {
    [IO.Directory]::CreateDirectory($parent) | Out-Null
}
[IO.File]::WriteAllText(
    $SignerPinOutputPath,
    "$signerPin`n",
    [Text.UTF8Encoding]::new($false)
)

if ($AllowMissingTimestamp) {
    Write-Host "Verified Authenticode signature and signer certificate pin $signerPin."
} else {
    Write-Host "Verified timestamped Authenticode signature and signer certificate pin $signerPin."
}
