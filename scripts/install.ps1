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

  Agent only, on a database host that accepts no inbound connections (it
  connects out to the DumpCat server):
    .\install.ps1 -Agent -Server https://backup.example.com -Key dca_...
                  [-Allow 127.0.0.1:3306] [-Insecure]
                  [-AllowPhysical]   # also run physical backups (mariabackup) here
                  [-AllowMSSQLDir D:\Backup\DumpCat]   # SQL Server: move its backup files for DumpCat (-Allow 127.0.0.1:1433)
    .\install.ps1 -Agent -Uninstall [-Purge]

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
  [switch]$Purge,
  [switch]$Agent,
  [string]$Server = '',
  [string]$Key = '',
  [string]$Allow = '',
  [switch]$Insecure,
  [switch]$AllowPhysical,
  [string]$AllowMSSQLDir = ''
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

# ---------- agent ----------
if ($Agent) {
  $AgentService = 'DumpCatAgent'
  $AgentEnv = Join-Path $DataRoot 'agent.env'
  if ($NoService) { Die '-Agent installs a service: run it without -NoService.' }
  $asvc = Get-Service -Name $AgentService -ErrorAction SilentlyContinue
  if ($asvc -and $asvc.Status -ne 'Stopped') {
    Stop-Service -Name $AgentService -Force
    $asvc.WaitForStatus('Stopped', (New-TimeSpan -Seconds 30))
  }
  if ($Uninstall) {
    Info 'Uninstalling the DumpCat agent'
    if ($asvc) { & sc.exe delete $AgentService | Out-Null }
    if (-not (Get-DumpCatService) -and (Test-Path $InstallDir)) { Remove-Item -Recurse -Force $InstallDir }
    if ($Purge) {
      Remove-Item -Force -ErrorAction SilentlyContinue $AgentEnv, (Join-Path $DataRoot 'agent.log*')
    } else {
      Info "Keeping $AgentEnv (use -Purge to delete it)"
    }
    exit 0
  }
  if ($Server -and $Server -notmatch '^https?://') { Die '-Server must be the DumpCat address, e.g. https://backup.example.com' }
  if (-not $Binary) { $Binary = Join-Path $PSScriptRoot 'dumpcat.exe' }
  if (-not (Test-Path $Binary)) { Die "dumpcat.exe not found: $Binary (use -Binary PATH)" }

  New-Item -ItemType Directory -Force -Path $InstallDir, $DataRoot | Out-Null
  if ((Resolve-Path $Binary).Path -ne [IO.Path]::GetFullPath($Exe)) {
    try { Copy-Item -Force $Binary $Exe }
    catch {
      if (-not (Test-Path $Exe)) { throw }
      Warn "Keeping the installed dumpcat.exe: it is in use ($($_.Exception.Message))"
    }
  }
  # only SYSTEM and Administrators may read the agent key
  & icacls.exe $DataRoot /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' /T /C /Q | Out-Null

  $vars = [ordered]@{}
  if (Test-Path $AgentEnv) {
    foreach ($line in Get-Content $AgentEnv) {
      if ($line -match '^\s*(DUMPCAT_AGENT_[A-Z_]+)\s*=(.*)$') { $vars[$Matches[1]] = $Matches[2].Trim() }
    }
  }
  if ($Server) { $vars['DUMPCAT_AGENT_SERVER'] = $Server.TrimEnd('/') }
  if ($Key) {
    if (-not $Key.StartsWith('dca_')) { Warn 'Agent keys start with dca_ - is this the key from the Agents page?' }
    $vars['DUMPCAT_AGENT_KEY'] = $Key
  }
  if ($Allow) { $vars['DUMPCAT_AGENT_ALLOW'] = $Allow }
  if ($Insecure) { $vars['DUMPCAT_AGENT_INSECURE'] = '1' }
  # the service runs as LocalSystem, which can read the datadir
  if ($AllowPhysical) { $vars['DUMPCAT_AGENT_ALLOW_PHYSICAL'] = '1' }
  # SQL Server's backup folder: LocalSystem reads and writes it; files the agent
  # creates there inherit the folder's permissions (SQL Server's account has Modify)
  if ($AllowMSSQLDir) {
    if (-not [IO.Path]::IsPathRooted($AllowMSSQLDir) -or $AllowMSSQLDir -notmatch '^([A-Za-z]:\\|\\\\)') { Die '-AllowMSSQLDir takes an absolute path, e.g. D:\Backup\DumpCat' }
    if (-not (Test-Path -LiteralPath $AllowMSSQLDir -PathType Container)) { Warn "$AllowMSSQLDir does not exist yet: create it and give SQL Server's service account Modify permission" }
    $vars['DUMPCAT_AGENT_MSSQL_DIR'] = $AllowMSSQLDir.TrimEnd('\')
  }
  if (-not $vars['DUMPCAT_AGENT_SERVER']) { Die '-Server is required, e.g. -Server https://backup.example.com' }
  if (-not $vars['DUMPCAT_AGENT_KEY']) { Die '-Key is required: the agent key from the Agents page of the web UI' }
  $lines = @('# DumpCat agent - after editing: Restart-Service DumpCatAgent')
  foreach ($k in $vars.Keys) { $lines += "$k=$($vars[$k])" }
  Info "Writing $AgentEnv"
  Write-Utf8NoBom $AgentEnv (($lines + '') -join "`r`n")
  $example = Join-Path $PSScriptRoot 'agent.env.example'
  if (Test-Path $example) { Copy-Item -Force $example (Join-Path $DataRoot 'agent.env.example') }

  $binPath = "`"$Exe`" agent -config `"$AgentEnv`""
  if ($asvc) {
    & sc.exe config $AgentService binPath= $binPath | Out-Null
  } else {
    Info "Creating the $AgentService service"
    New-Service -Name $AgentService -BinaryPathName $binPath -DisplayName 'DumpCat agent' `
      -Description 'Connects this database host out to a DumpCat server' -StartupType Automatic | Out-Null
  }
  & sc.exe failure $AgentService reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null
  & sc.exe failureflag $AgentService 1 | Out-Null
  $log = Join-Path $DataRoot 'agent.log'
  $logLines = 0 # only look at what this start writes
  if (Test-Path $log) { $logLines = @(Get-Content $log).Count }
  Info "Starting the $AgentService service"
  Start-Service -Name $AgentService
  $connected = $false
  for ($i = 0; $i -lt 15 -and -not $connected; $i++) {
    Start-Sleep -Seconds 1
    if ((Get-Service -Name $AgentService).Status -ne 'Running') { break }
    if ((Test-Path $log) -and (Get-Content $log | Select-Object -Skip $logLines | Select-String -SimpleMatch 'connected to ')) { $connected = $true }
  }
  if ((Get-Service -Name $AgentService).Status -ne 'Running') {
    if (Test-Path $log) { Get-Content $log -Tail 20 }
    Die "The agent failed to start - see $log"
  }
  Write-Host ''
  if ($connected) {
    Write-Host 'The DumpCat agent is connected. It shows as online on the Agents page of the web UI.' -ForegroundColor Cyan
  } else {
    if (Test-Path $log) { Get-Content $log -Tail 10 }
    Warn 'The agent is running but has not connected yet - check the address, the key and that this host can reach the server.'
  }
  Write-Host 'Next, in the web UI: Database servers > Add server > Connect: through this agent, with the database'
  Write-Host 'address as seen from this host (e.g. 127.0.0.1 port 3306).'
  if ($vars['DUMPCAT_AGENT_ALLOW_PHYSICAL'] -eq '1') {
    Write-Host 'Physical backups: the Agents page shows whether mariabackup and the datadir were found.'
  }
  if ($vars['DUMPCAT_AGENT_MSSQL_DIR']) {
    Write-Host "SQL Server: enter $($vars['DUMPCAT_AGENT_MSSQL_DIR']) as the server's backup folder (as SQL Server sees it) and leave"
    Write-Host '"As DumpCat sees it" empty - the agent moves the files. Allow the instance: -Allow 127.0.0.1:1433.'
  }
  Write-Host ''
  Write-Host "Configuration: $AgentEnv  (after editing: Restart-Service $AgentService)"
  Write-Host "Log:           $log"
  exit 0
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
  # the service writes the one-time setup token while it starts
  $token = $null
  if (-not $NoService) {
    for ($i = 0; $i -lt 10 -and -not $token; $i++) {
      try { $token = & $Exe setup-token -config $EnvFile 2>$null } catch { $token = $null }
      if (-not $token) { Start-Sleep -Seconds 1 }
    }
  }
  Write-Host 'No account yet: open the address above and create the admin account.' -ForegroundColor Cyan
  $where = 'it is also in the log below'
  if ($token) {
    Write-Host '  The setup page asks for this one-time setup token: ' -NoNewline
    Write-Host $token -ForegroundColor Cyan
  } elseif ($NoService) {
    Write-Host '  The setup page asks for a one-time setup token.'
    $where = 'DumpCat also prints it when it starts'
  }
  Write-Host "  Show the token: & `"$Exe`" setup-token -config `"$EnvFile`"  ($where)"
  Write-Host "  or create an admin from the command line:  & `"$Exe`" user add -u admin -r admin -config `"$EnvFile`""
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
