# DumpCat one-line installer for Windows, served at https://dumpcat.cc/install.ps1
#
# In PowerShell opened as Administrator:
#   irm https://dumpcat.cc/install.ps1 | iex
#
# Options of the package's install.ps1 (-Port 9090, -OpenFirewall, -Uninstall...):
#   & ([scriptblock]::Create((irm https://dumpcat.cc/install.ps1))) -Port 9090 -OpenFirewall
#
# Downloads the latest release package from downloads.dumpcat.cc, checks its SHA-256 against the release's
# SHA256SUMS, unpacks it in a temporary folder and runs the install.ps1 inside it with the same options.
# The temporary folder is removed afterwards. $env:DUMPCAT_VERSION installs a given version.

& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is much slower with the progress bar
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

  function Say([string]$m) { Write-Host "==> $m" -ForegroundColor Green }
  # throw, not exit: with irm | iex, exit would close the user's PowerShell window
  function Die([string]$m) { Write-Host "xx  $m" -ForegroundColor Red; throw 'DumpCat was not installed.' }

  $base = 'https://downloads.dumpcat.cc'
  if ($env:DUMPCAT_DOWNLOAD_URL) { $base = $env:DUMPCAT_DOWNLOAD_URL.TrimEnd('/') }

  $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
  if (-not $admin) { Die 'Please open PowerShell as Administrator (right-click > Run as administrator) and run the command again.' }
  if (-not [Environment]::Is64BitOperatingSystem) { Die 'DumpCat needs 64-bit Windows.' }

  $version = $env:DUMPCAT_VERSION
  if (-not $version) {
    $latest = Invoke-RestMethod -UseBasicParsing "$base/latest.json"
    $version = $latest.version
    if (-not $version) { Die "No version in $base/latest.json" }
  }
  $pkg = "dumpcat-$version-windows-amd64"

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ("dumpcat-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Say "Downloading DumpCat $version"
    $zip = Join-Path $tmp "$pkg.zip"
    Invoke-WebRequest -UseBasicParsing "$base/$version/$pkg.zip" -OutFile $zip
    $sums = (Invoke-WebRequest -UseBasicParsing "$base/$version/SHA256SUMS").Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }

    $want = $null
    foreach ($line in ($sums -split "`n")) {
      $f = $line.Trim() -split '\s+'
      if ($f.Count -ge 2 -and $f[1].TrimStart('*') -eq "$pkg.zip") { $want = $f[0].ToLower() }
    }
    if (-not $want) { Die "$pkg.zip is not listed in SHA256SUMS" }
    $got = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
    if ($got -ne $want) { Die "Checksum mismatch for $pkg.zip: the download is damaged, please try again." }
    Say 'Checksum OK'

    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $installer = Join-Path (Join-Path $tmp $pkg) 'install.ps1'
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $installer @args
    if ($LASTEXITCODE) { Die "install.ps1 failed (exit code $LASTEXITCODE)" }
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
} @args
