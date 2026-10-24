@echo off
rem Register BargainProxy gateway models into the local WorkBuddy desktop
rem app: writes the user models.json after a timestamped backup, and the
rem WorkBuddy app picks it up within a second. No restart needed.
rem
rem Prerequisite: the gateway must be running - scripts\start-all.cmd
rem Double-click to run. Default applies the changes; pass "preview" to
rem dry-run.
setlocal
set "ROOT=%~dp0.."
pushd "%ROOT%"

where go >nul 2>nul
if errorlevel 1 (
  echo [wbmodels] go not found. Install Go, or use the panel button instead.
  pause
  exit /b 1
)

echo [wbmodels] Registering gateway models into WorkBuddy ...
if /i "%~1"=="preview" (
  go run .\scripts\wbmodels
) else (
  go run .\scripts\wbmodels -write
)
if errorlevel 1 (
  echo.
  echo [wbmodels] Failed. Most common cause: gateway not running.
  echo [wbmodels] Start it first: scripts\start-all.cmd
  pause
  exit /b 1
)
echo.
echo [wbmodels] Done. WorkBuddy loads it within a second.
pause
