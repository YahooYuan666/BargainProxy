@echo off
rem Start the project-local portable PostgreSQL. Data: .tools\pgdata
rem Listens on 127.0.0.1:55432 only. No system installation involved.
setlocal
set "ROOT=%~dp0..\.."
pushd "%ROOT%"

set "PGBIN=%ROOT%\.tools\postgres\bin"
set "PGDATA=%ROOT%\.tools\pgdata"
set "PGLOG=%ROOT%\.tools\pg.log"

if not exist "%PGBIN%\pg_ctl.exe" goto missing

if not exist "%PGDATA%\PG_VERSION" (
  echo [pg] first run: initializing data directory...
  "%PGBIN%\initdb.exe" -D "%PGDATA%" -U bargainproxy -A trust -E UTF8 --locale=C
  if errorlevel 1 goto failed
)

"%PGBIN%\pg_ctl.exe" -D "%PGDATA%" status >nul 2>&1
if errorlevel 1 (
rem Start detached from this console: closing the launcher window must not
rem deliver CTRL_CLOSE to postgres (that killed the DB at 2026-10-20 23:09).
powershell -NoProfile -ExecutionPolicy Bypass -File "%ROOT%\scripts\pg\start-detached.ps1" -PgBin "%PGBIN%" -Data "%PGDATA%" -Log "%PGLOG%"
if errorlevel 1 goto failed
echo [pg] started on 127.0.0.1:55432
) else (
  echo [pg] already running on 127.0.0.1:55432
)

rem The application database is created by the gateway itself on first run
rem (store.EnsureDatabase), so this step needs no Go toolchain - boot autostart
rem runs with a bare PATH (2026-10-21).
echo [pg] ready. dsn: postgres://bargainproxy@127.0.0.1:55432/bargainproxy?sslmode=disable
popd
exit /b 0

:missing
echo [pg] portable binaries missing: %PGBIN%
echo [pg] re-download per README section "portable PostgreSQL".
popd
exit /b 1

:failed
echo [pg] start failed. see %PGLOG%
popd
exit /b 1
