@echo off
rem One-click stop for BargainProxy: gateway (port 13140) then portable PG.
setlocal
set "ROOT=%~dp0.."
pushd "%ROOT%"

echo [1/2] gateway (port 13140)...
powershell -NoProfile -Command "$c = Get-NetTCPConnection -LocalPort 13140 -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1; if ($c) { Stop-Process -Id $c.OwningProcess -Force; Write-Host ('[ok] stopped gateway pid ' + $c.OwningProcess) } else { Write-Host '[ok] gateway not running' }"

echo [2/2] portable PostgreSQL...
if exist "%ROOT%\scripts\pg\stop.cmd" call "%ROOT%\scripts\pg\stop.cmd"

popd
endlocal
exit /b 0
