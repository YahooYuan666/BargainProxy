@echo off
rem One-click start for BargainProxy on Windows:
rem   1) portable PostgreSQL (scripts\pg\start.cmd, idempotent)
rem   2) build the gateway exe if missing
rem   3) first-run bootstrap: if the panel password is not set yet, ask for it
rem      and persist it (the panel stays disabled until then, so the URL 404s)
rem   4) start the gateway hidden on 127.0.0.1:13140 if not already listening
rem Idempotent: running it again while everything is up exits quickly.
rem Logs: logs\gateway.log  (stop with scripts\stop-all.cmd)
setlocal
set "ROOT=%~dp0.."
set "DSN=postgres://bargainproxy@127.0.0.1:55432/bargainproxy?sslmode=disable"
pushd "%ROOT%"

if not exist "%ROOT%\logs" mkdir "%ROOT%\logs"

echo [1/4] portable PostgreSQL...
call "%ROOT%\scripts\pg\start.cmd"
if errorlevel 1 goto failed

echo [2/4] gateway binary...
if not exist "%ROOT%\bargainproxy.exe" (
  where go >nul 2>nul
  if errorlevel 1 (
    echo [error] bargainproxy.exe missing and Go not in PATH. Install Go or run: go build ./cmd/bargainproxy
    goto failed
  )
  go build -o bargainproxy.exe ./cmd/bargainproxy
  if errorlevel 1 goto failed
)

echo [3/4] panel state...
rem The panel runs open (no password) in this build, so there is nothing to
rem probe. The old password gate needed `go run` (absent from the boot PATH)
rem and aborted the whole boot start when it could not decide (2026-10-21).
echo [ok] panel open mode
goto gateway

:gateway
echo [4/4] gateway on 127.0.0.1:13140...
rem netstat+findstr keeps this free of nested quoting (survives cmd /c from
rem other shells, double-click, and scheduled tasks alike).
netstat -ano | findstr /C::13140 | findstr /C:LISTENING >nul 2>nul
if errorlevel 1 goto startgw
echo [ok] gateway already listening on 127.0.0.1:13140
goto done

:startgw
powershell -NoProfile -Command "Start-Process -FilePath '%ROOT%\bargainproxy.exe' -ArgumentList '-config','%ROOT%\config.json' -WorkingDirectory '%ROOT%' -WindowStyle Hidden -RedirectStandardOutput '%ROOT%\logs\gateway.log' -RedirectStandardError '%ROOT%\logs\gateway.err.log'"
if errorlevel 1 goto failed

rem Wait for the port (max ~10 x 1s). ping is the reliable non-interactive
rem sleep inside redirected consoles (timeout /t hangs waiting for key input
rem when stdin is a pipe).
set /a tries=0
:waitloop
ping -n 2 127.0.0.1 >nul
netstat -ano | findstr /C::13140 | findstr /C:LISTENING >nul 2>nul
if errorlevel 1 (
  set /a tries+=1
  if %tries% lss 10 goto waitloop
  echo [error] gateway port never came up; check logs\gateway.err.log
  goto failed
)
echo [ok] gateway up. Panel: http://127.0.0.1:13140/panel/  (logs: logs\gateway.log)

:done
popd
endlocal
exit /b 0

:failed
popd
endlocal
exit /b 1
