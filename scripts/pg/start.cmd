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
  "%PGBIN%\pg_ctl.exe" -D "%PGDATA%" -l "%PGLOG%" -o "-p 55432 -c listen_addresses=127.0.0.1" start
  if errorlevel 1 goto failed
  echo [pg] started on 127.0.0.1:55432
) else (
  echo [pg] already running on 127.0.0.1:55432
)

rem Create the application database if missing. Idempotent. Needs Go.
where go >nul 2>nul
if errorlevel 1 goto dsn
go run ./scripts/pginit -dsn "postgres://bargainproxy@127.0.0.1:55432/postgres?sslmode=disable" -db bargainproxy

:dsn
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
