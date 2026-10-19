@echo off
rem Double-click to install DumpCat (or upgrade it) as a Windows service.
rem Windows first asks for administrator rights. Same as running install.ps1 in an elevated PowerShell.
setlocal
cd /d "%~dp0"
net session >nul 2>&1
if errorlevel 1 (
  echo Asking Windows for administrator rights...
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
if errorlevel 1 (
  echo.
  echo DumpCat was not installed: see the messages above.
  pause
  exit /b 1
)
echo.
echo Opening DumpCat in your browser...
start "" "http://localhost:6369/"
echo Keep the setup token above: the setup page asks for it. Press a key to close this window.
pause >nul
