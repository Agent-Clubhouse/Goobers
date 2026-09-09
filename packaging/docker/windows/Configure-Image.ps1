$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# This build-only step runs as the base's ContainerAdministrator. Runtime uses
# ContainerUser; only HOME, its temp directory and workspace receive write access.
foreach ($path in @($env:HOME, $env:TEMP, 'C:\workspace')) {
    New-Item -ItemType Directory -Force -Path $path | Out-Null
    & icacls.exe $path /grant '*S-1-5-93-2-2:(OI)(CI)M' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Cannot grant ContainerUser access to $path" }
}
# The versioned MinGit bundle is also the trust source for Windows-native Go TLS.
# Import public roots during the build; no user certificates or credentials enter
# the context. Preserve MinGit's license files with the distribution.
$pem = Get-Content -LiteralPath $env:SSL_CERT_FILE -Raw
$certificates = [regex]::Matches($pem, '-----BEGIN CERTIFICATE-----\s*(.*?)\s*-----END CERTIFICATE-----', 'Singleline')
if ($certificates.Count -eq 0) { throw 'CA bundle contains no certificates' }
$store = New-Object System.Security.Cryptography.X509Certificates.X509Store('Root', 'LocalMachine')
$store.Open('ReadWrite')
try {
    foreach ($certificate in $certificates) {
        $der = [Convert]::FromBase64String($certificate.Groups[1].Value)
        $store.Add((New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 -ArgumentList @(,$der)))
    }
} finally { $store.Close() }
# Replace installer defaults (credential manager, absent LFS filters and host
# config includes). Public CA configuration is the only provider-related state.
@'
[core]
    autocrlf = false
    symlinks = false
[http]
    sslBackend = openssl
    sslCAInfo = C:/Git/mingw64/etc/ssl/certs/ca-bundle.crt
'@ | Set-Content -LiteralPath C:\Git\etc\gitconfig -Encoding Ascii
