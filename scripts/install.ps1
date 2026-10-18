<#
.SYNOPSIS
  DumpCat installer for Windows - MySQL/MariaDB backup & restore manager.

.DESCRIPTION
  Installs dumpcat.exe as a Windows service (auto start, restart on failure),
  creates the configuration and data folders and downloads rclone.

  Run from an elevated PowerShell in the unzipped folder:
    .\install.ps1                      # install or upgrade, web UI on port 6369
    .\install.ps1 -Port 9090 -OpenFirewall
    .\install.ps1 -TlsAuto             # HTTPS with a self-signed certificate
    .\install.ps1 -NoService           # copy files and write the config only (no admin rights needed)
    .\install.ps1 -Uninstall [-Purge]

  If scripts are blocked: powershell -ExecutionPolicy Bypass -File .\install.ps1
#>
[CmdletBinding()]
param(
  [int]$Port = 6369,
  [string]$Listen = '0.0.0.0',
  [switch]$TlsAuto,
  [string]$InstallDir = (Join-Path $env:ProgramFiles 'DumpCat'),
  [string]$DataRoot = (Join-Path $env:ProgramData 'DumpCat'),
  [string]$Binary = '',
  [switch]$OpenFirewall,
  [switch]$NoRclone,
  [switch]$NoService,
  [switch]$Uninstall,
  [switch]$Purge
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'DumpCat'
$FirewallRule = 'DumpCat-HTTP'
$Exe = Join-Path $InstallDir 'dumpcat.exe'
$ToolsDir = Join-Path $InstallDir 'tools'
$DataDir = Join-Path $DataRoot 'data'
$EnvFile = Join-Path $DataRoot 'dumpcat.env'

function Info([string]$m) { Write-Host "==> $m" -ForegroundColor Green }
function Warn([string]$m) { Write-Host "!!  $m" -ForegroundColor Yellow }
function Die([string]$m) { Write-Host "xx  $m" -ForegroundColor Red; exit 1 }

# The env file is read by Go: write UTF-8 without a BOM.
function Write-Utf8NoBom([string]$Path, [string]$Text) {
  [System.IO.File]::WriteAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  return (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-DumpCatService { Get-Service -Name $ServiceName -ErrorAction SilentlyContinue }

function Stop-DumpCat {
  $svc = Get-DumpCatService
  if ($svc -and $svc.Status -ne 'Stopped') {
    Info "Stopping the $ServiceName service"
    Stop-Service -Name $ServiceName -Force
    $svc.WaitForStatus('Stopped', (New-TimeSpan -Seconds 90))
  }
}

if (-not $NoService -and -not (Test-Admin)) {
  Die 'Please run from an elevated PowerShell (Run as administrator), or use -NoService.'
}

# ---------- uninstall ----------
if ($Uninstall) {
  Info 'Uninstalling DumpCat'
  if (-not $NoService) {
    Stop-DumpCat
    if (Get-DumpCatService) {
      & sc.exe delete $ServiceName | Out-Null
      Info "Removed the $ServiceName service"
    }
    Get-NetFirewallRule -Name $FirewallRule -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  }
  if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
  if ($Purge) {
    Warn "Deleting data and configuration: $DataRoot (backups on your storages are NOT deleted)"
    if (Test-Path $DataRoot) { Remove-Item -Recurse -Force $DataRoot }
  } else {
    Info "Keeping data and configuration in $DataRoot (use -Purge to delete them)"
  }
  exit 0
}

# ---------- files ----------
if (-not $Binary) { $Binary = Join-Path $PSScriptRoot 'dumpcat.exe' }
if (-not (Test-Path $Binary)) { Die "dumpcat.exe not found: $Binary (use -Binary PATH)" }

if (-not $NoService) { Stop-DumpCat }

Info "Installing to $InstallDir"
New-Item -ItemType Directory -Force -Path $InstallDir, $ToolsDir | Out-Null
if ((Resolve-Path $Binary).Path -ne [IO.Path]::GetFullPath($Exe)) { Copy-Item -Force $Binary $Exe }
foreach ($f in 'README.md', 'README.vi.md', 'CONTRIBUTING.md', 'SECURITY.md', 'LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'docs\manual.md', 'docs\manual.vi.md') {
  $src = Join-Path $PSScriptRoot $f
  if (Test-Path $src) {
    $dst = Join-Path $InstallDir $f
    New-Item -ItemType Directory -Force -Path (Split-Path $dst) | Out-Null
    Copy-Item -Force $src $dst
  }
}

New-Item -ItemType Directory -Force -Path $DataRoot, $DataDir | Out-Null
if (-not $NoService) {
  # The service runs as LocalSystem: only SYSTEM and Administrators may read
  # the database, the master key and the logs.
  & icacls.exe $DataRoot /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' /T /C /Q | Out-Null
  if ($LASTEXITCODE -ne 0) { Warn "Could not restrict the permissions of $DataRoot" }
}

if (-not (Test-Path $EnvFile)) {
  Info "Creating configuration $EnvFile"
  $tls = 0
  if ($TlsAuto) { $tls = 1 }
  Write-Utf8NoBom $EnvFile (@(
      '# DumpCat configuration - see dumpcat.env.example for all options',
      '# After editing: restart DumpCat (as a service: Restart-Service DumpCat)',
      "DUMPCAT_LISTEN=${Listen}:${Port}",
      "DUMPCAT_DATA_DIR=$DataDir",
      "DUMPCAT_TLS_AUTO=$tls",
      '#DUMPCAT_BASE_URL=https://backup.example.com',
      '#DUMPCAT_TRUST_PROXY=1',
      '# extra folders with mysqldump.exe / mariadb-dump.exe ... (separated by ;)',
      '#DUMPCAT_TOOLS_DIR=C:\path\to\mysql\bin',
      '') -join "`r`n")
} else {
  Info "Keeping the existing configuration $EnvFile"
}
$example = Join-Path $PSScriptRoot 'dumpcat.env.example'
if (Test-Path $example) { Copy-Item -Force $example (Join-Path $DataRoot 'dumpcat.env.example') }

# ---------- rclone (cloud storages) ----------
$rclone = Join-Path $ToolsDir 'rclone.exe'
if (-not $NoRclone -and -not (Test-Path $rclone)) {
  Info 'Downloading rclone (S3, Google Drive, OneDrive, FTP, SFTP...)'
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('dumpcat-rclone-' + [guid]::NewGuid().ToString('N'))
  try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null
    $zip = Join-Path $tmp 'rclone.zip'
    $ProgressPreference = 'SilentlyContinue'
    Invoke-WebRequest -UseBasicParsing -Uri 'https://downloads.rclone.org/rclone-current-windows-amd64.zip' -OutFile $zip
    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $found = Get-ChildItem -Path $tmp -Recurse -Filter 'rclone.exe' | Select-Object -First 1
    if (-not $found) { throw 'rclone.exe not found in the archive' }
    Copy-Item -Force $found.FullName $rclone
    Info "rclone installed: $rclone"
  } catch {
    Warn "Could not download rclone ($($_.Exception.Message)). Cloud storages need it: put rclone.exe into $ToolsDir"
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
}

# ---------- service ----------
$scheme = 'http'
if ($TlsAuto -or ((Get-Content $EnvFile -ErrorAction SilentlyContinue) -match '^\s*DUMPCAT_TLS_AUTO\s*=\s*(1|true|yes|on)\s*$')) { $scheme = 'https' }

if ($NoService) {
  Write-Host ''
  Info 'Files and configuration are in place (no service was created). Start DumpCat with:'
  Write-Host "  & `"$Exe`" serve -config `"$EnvFile`""
} else {
  $binPath = "`"$Exe`" serve -config `"$EnvFile`""
  if (Get-DumpCatService) {
    & sc.exe config $ServiceName binPath= $binPath | Out-Null
  } else {
    Info "Creating the $ServiceName service"
    New-Service -Name $ServiceName -BinaryPathName $binPath -DisplayName 'DumpCat (MySQL/MariaDB backup)' `
      -Description 'MySQL/MariaDB backup & restore manager with a web UI' -StartupType Automatic | Out-Null
  }
  # start after the database services; restart automatically if it crashes
  & sc.exe config $ServiceName start= delayed-auto | Out-Null
  & sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null
  # also when it stops with an error code (a new version that cannot start is then rolled back)
  & sc.exe failureflag $ServiceName 1 | Out-Null

  if ($OpenFirewall) {
    $rule = Get-NetFirewallRule -Name $FirewallRule -ErrorAction SilentlyContinue
    if ($rule) {
      $rule | Get-NetFirewallPortFilter | Set-NetFirewallPortFilter -LocalPort $Port
    } else {
      New-NetFirewallRule -Name $FirewallRule -DisplayName 'DumpCat web UI' -Direction Inbound -Protocol TCP `
        -LocalPort $Port -Action Allow -Profile Domain, Private | Out-Null
    }
    Info "Firewall: opened TCP $Port (Domain and Private networks)"
  }

  Info "Starting the $ServiceName service"
  Start-Service -Name $ServiceName
  Start-Sleep -Seconds 3
  $svc = Get-DumpCatService
  if (-not $svc -or $svc.Status -ne 'Running') {
    $log = Join-Path $DataDir 'dumpcat.log'
    if (Test-Path $log) { Get-Content $log -Tail 30 }
    Die "The service failed to start - see $log"
  }
}

# ---------- summary ----------
$hostName = $env:COMPUTERNAME
$ip = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
  Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } |
  Select-Object -First 1 -ExpandProperty IPAddress
if ($ip) { $hostName = $ip }
Write-Host ''
$url = "${scheme}://${hostName}:${Port}/  (on this machine: ${scheme}://localhost:${Port}/)"
if ($NoService) {
  Write-Host "Once started, DumpCat is at: $url" -ForegroundColor Cyan
} else {
  Write-Host "DumpCat is running: $url" -ForegroundColor Cyan
}
$users = & $Exe user list -config $EnvFile 2>$null
if (-not $users) {
  Write-Host 'No account yet: open the address above now and create the admin account' -ForegroundColor Cyan
  Write-Host '  (until then, the first visitor becomes the administrator).'
  Write-Host "  or from the command line:  & `"$Exe`" user add -u admin -r admin -config `"$EnvFile`""
}
if (-not $OpenFirewall -and -not $NoService) {
  Write-Host "To reach the web UI from other machines, open TCP $Port in Windows Firewall (or re-run with -OpenFirewall)."
}
Write-Host ''
if ($NoService) {
  Write-Host "Configuration: $EnvFile  (restart DumpCat after editing)"
} else {
  Write-Host "Configuration: $EnvFile  (after editing: Restart-Service $ServiceName)"
  Write-Host "Log:           $(Join-Path $DataDir 'dumpcat.log')"
}
Write-Host "Tools folder:  $ToolsDir  (MySQL/MariaDB bin folders under Program Files are found automatically)"
Write-Host 'Check the installed tools on the System page of the web UI.'
