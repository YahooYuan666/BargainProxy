@echo off
rem Thin wrapper so the check is reachable from cmd / double-click.
rem The logic lives in verify-deploy.ps1 (cmd mangles the quoting).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0verify-deploy.ps1"
