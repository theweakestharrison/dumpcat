# Build the Linux and Windows release packages and the update folder from Windows (requires Go in PATH):
#   .\build.ps1
#   .\build.ps1 -UpdateUrl https://downloads.dumpcat.cc   # built-in release server
#   .\build.ps1 -Version 1.1.0 -SkipTests
#   .\build.ps1 -SigningKey D:\keys\dumpcat-release.key   # sign latest.json, trust that key ($env:UPDATE_SIGNING_KEY)
#   .\build.ps1 -PublicKeys KEY[,KEY]                       # also trust these public keys ($env:UPDATE_PUBLIC_KEYS, key rotation)
param(
    [string]$Version = '',
    [string]$UpdateUrl = $env:UPDATE_URL,
    [string]$SigningKey = $env:UPDATE_SIGNING_KEY,
    [string]$PublicKeys = $env:UPDATE_PUBLIC_KEYS,
    [switch]$SkipTests
)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
if (-not $Version) { $Version = (Get-Content VERSION -ErrorAction SilentlyContinue | Select-Object -First 1) }
if (-not $Version) { $Version = 'dev' }
$ldflags = "-s -w -X main.version=$Version"
if ($UpdateUrl) { $ldflags += " -X main.defaultUpdateURL=$UpdateUrl" }
if ($SigningKey -or $PublicKeys) {
    # the binaries only install releases signed by one of these keys (validated here)
    $keyArgs = @()
    if ($SigningKey) { $keyArgs += @('-key', $SigningKey) }
    if ($PublicKeys) { $keyArgs += @($PublicKeys -split '[,;\s]+' | Where-Object { $_ }) }
    $keys = go run ./cmd/dumpcat release pubkey @keyArgs
    if ($LASTEXITCODE -ne 0) { throw 'invalid release signing key / public keys' }
    $keys = "$($keys | Select-Object -Last 1)".Trim()
    $ldflags += " -X main.updatePublicKeys=$keys"
    Write-Host "==> trusted release keys: $keys"
}
New-Item -ItemType Directory -Force dist | Out-Null
if (-not $SkipTests) {
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'tests failed' }
}
# the packages ship THIRD_PARTY_NOTICES.md; it must match go.mod (regenerate: go run ./scripts/notices)
go run ./scripts/notices -check
if ($LASTEXITCODE -ne 0) { throw 'THIRD_PARTY_NOTICES.md is out of date' }
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
foreach ($arch in 'amd64', 'arm64') {
    Write-Host "==> linux/$arch"
    $env:GOARCH = $arch
    go build -trimpath -ldflags $ldflags -o "dist/dumpcat-linux-$arch" ./cmd/dumpcat
    if ($LASTEXITCODE -ne 0) { throw "build $arch failed" }
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
$pkg = "dumpcat-$Version-linux"
$stage = Join-Path dist $pkg
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force $stage | Out-Null
Copy-Item dist/dumpcat-linux-amd64, dist/dumpcat-linux-arm64, scripts/install.sh, deploy/dumpcat.service, deploy/dumpcat.env.example, deploy/nginx-dumpcat.conf, README.md, README.vi.md, CHANGELOG.md, CONTRIBUTING.md, SECURITY.md, LICENSE, NOTICE, THIRD_PARTY_NOTICES.md $stage
New-Item -ItemType Directory -Force (Join-Path $stage docs) | Out-Null
Copy-Item docs/manual.md, docs/manual.vi.md (Join-Path $stage docs)
tar --uid 0 --gid 0 --uname root --gname root -C dist -czf "dist/$pkg.tar.gz" $pkg
Remove-Item -Recurse -Force $stage
Write-Host "==> dist/$pkg.tar.gz"

Write-Host "==> windows/amd64"
$env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = 'amd64'
go build -trimpath -ldflags $ldflags -o dist/dumpcat-windows-amd64.exe ./cmd/dumpcat
if ($LASTEXITCODE -ne 0) { throw 'build windows failed' }
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
$wpkg = "dumpcat-$Version-windows-amd64"
$wstage = Join-Path dist $wpkg
if (Test-Path $wstage) { Remove-Item -Recurse -Force $wstage }
New-Item -ItemType Directory -Force $wstage | Out-Null
Copy-Item dist/dumpcat-windows-amd64.exe (Join-Path $wstage 'dumpcat.exe')
Copy-Item scripts/install.ps1, deploy/dumpcat.env.example, README.md, README.vi.md, CHANGELOG.md, CONTRIBUTING.md, SECURITY.md, LICENSE, NOTICE, THIRD_PARTY_NOTICES.md $wstage
New-Item -ItemType Directory -Force (Join-Path $wstage docs) | Out-Null
Copy-Item docs/manual.md, docs/manual.vi.md (Join-Path $wstage docs)
if (Test-Path "dist/$wpkg.zip") { Remove-Item -Force "dist/$wpkg.zip" }
Compress-Archive -Path $wstage -DestinationPath "dist/$wpkg.zip"
Remove-Item -Recurse -Force $wstage
Write-Host "==> dist/$wpkg.zip"

function Get-Sums($files) {
    Get-FileHash $files -Algorithm SHA256 | ForEach-Object { "{0}  {1}" -f $_.Hash.ToLower(), (Split-Path $_.Path -Leaf) }
}
Get-Sums @('dist/dumpcat-linux-amd64', 'dist/dumpcat-linux-arm64', 'dist/dumpcat-windows-amd64.exe', "dist/$pkg.tar.gz", "dist/$wpkg.zip") |
    Set-Content -Encoding ascii dist/SHA256SUMS

# ---- update folder: latest.json + <version>/ (upload to the release server) ----
$rel = Join-Path dist "update/$Version"
if (Test-Path $rel) { Remove-Item -Recurse -Force $rel }
if (Test-Path dist/update/latest.json.sig) { Remove-Item -Force dist/update/latest.json.sig }
New-Item -ItemType Directory -Force $rel | Out-Null
Copy-Item dist/dumpcat-linux-amd64, dist/dumpcat-linux-arm64, dist/dumpcat-windows-amd64.exe, "dist/$pkg.tar.gz", "dist/$wpkg.zip" $rel
Get-Sums (Get-ChildItem $rel -File | ForEach-Object FullName) | Set-Content -Encoding ascii (Join-Path $rel 'SHA256SUMS')

# release notes: the "## <version>" section of CHANGELOG.md
$notes = @()
$on = $false
foreach ($line in (Get-Content CHANGELOG.md -ErrorAction SilentlyContinue)) {
    if ($line -like '## *') { if ($on) { break }; $on = $line.StartsWith("## $Version"); continue }
    if ($on) { $notes += $line }
}
$files = [ordered]@{}
foreach ($p in @(@('linux/amd64', 'dumpcat-linux-amd64'), @('linux/arm64', 'dumpcat-linux-arm64'), @('windows/amd64', 'dumpcat-windows-amd64.exe'))) {
    $f = Join-Path $rel $p[1]
    $files[$p[0]] = [ordered]@{ url = "$Version/$($p[1])"; sha256 = (Get-FileHash $f -Algorithm SHA256).Hash.ToLower(); size = (Get-Item $f).Length }
}
$latest = [ordered]@{
    version     = $Version
    released_at = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    notes       = (($notes -join "`n").Trim())
    files       = $files
}
[System.IO.File]::WriteAllText((Join-Path $PSScriptRoot 'dist/update/latest.json'), ($latest | ConvertTo-Json -Depth 5), (New-Object System.Text.UTF8Encoding($false)))
Write-Host "==> dist/update/latest.json ($Version)"
if ($SigningKey) {
    go run ./cmd/dumpcat release sign -key $SigningKey dist/update/latest.json
    if ($LASTEXITCODE -ne 0) { throw 'signing latest.json failed' }
} elseif ($PublicKeys) {
    Write-Warning 'latest.json is not signed - installations that trust a key will refuse it. Sign it before uploading: dumpcat release sign -key FILE dist/update/latest.json'
}
Write-Host "==> upload dist/update/ to the release server (the version folder first, latest.json.sig and latest.json last)"
