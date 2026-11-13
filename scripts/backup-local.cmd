@echo off
rem Entry point for the local-asset backup. The real logic lives in
rem backup-local.ps1: cmd's quoting rules make for/f + inline PowerShell
rem fragile, and this script must never fail silently (a broken backup that
rem looks successful is worse than no backup).
rem
rem NOTE: keep this file pure ASCII. An em dash or any multi-byte character
rem is split by the GBK console code page and cmd tries to run the orphan
rem byte as a command ("'m' is not recognized").
rem
rem Usage:  scripts\backup-local.cmd [destination-dir]
rem         default destination: D:\Backup\BargainProxy
setlocal
set "HERE=%~dp0"
powershell -NoProfile -ExecutionPolicy Bypass -File "%HERE%backup-local.ps1" %*
set "RC=%errorlevel%"
if not "%RC%"=="0" (
  echo [error] backup failed with exit code %RC%
  endlocal
  exit /b %RC%
)
endlocal
exit /b 0
