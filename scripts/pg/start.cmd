@echo off
setlocal enabledelayedexpansion
rem Start the project-local portable PostgreSQL. Data: .tools\pgdata
rem Listens on 127.0.0.1:55432 only. No system installation involved.
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

rem 防御性兜底：确保 PGDATA 下的标准空子目录齐全（从其他设备拷贝、解压或 Git 检出时常丢失空目录，导致 pg_ctl 报 FATAL: could not open directory）
for %%d in (pg_commit_ts pg_dynshmem pg_logical\mappings pg_logical\snapshots pg_notify pg_replslot pg_serial pg_snapshots pg_stat pg_stat_tmp pg_tblspc pg_twophase pg_wal\archive_status) do (
  if not exist "%PGDATA%\%%d" mkdir "%PGDATA%\%%d" >nul 2>&1
)

"%PGBIN%\pg_ctl.exe" -D "%PGDATA%" status >nul 2>&1
if not errorlevel 1 (
  echo [pg] already running on 127.0.0.1:55432
  goto pg_ready
)

rem A just-stopped server keeps its data directory locked for a few seconds
rem after the port closes. Starting in that window makes pg_ctl wait about
rem 30s and then fail (observed 2026-10-22: the first start-all after a stop
rem always died at this step, the second one worked). Wait out the old
rem process first.
rem Wait by PID, not by pg_ctl's text: while a server is shutting down its
rem status line says "server is shutting down" — which does NOT contain
rem "no server" — so a text match loops forever and the window freezes on
rem the first line (observed 2026-10-22 when double-clicking start-all).
set /a waits=0
:waitold
set "OLDPID="
if exist "%PGDATA%\postmaster.pid" (
  for /f "usebackq" %%a in ("%PGDATA%\postmaster.pid") do (
    set "OLDPID=%%a"
    goto checkpid
  )
)
:checkpid
if not defined OLDPID goto startdb
tasklist /FI "PID eq !OLDPID!" 2>nul | findstr /C:" !OLDPID! " >nul
if not errorlevel 1 (
  set /a waits+=1
  if !waits! lss 20 (ping -n 2 127.0.0.1 >nul & goto waitold)
)
:startdb
rem Start detached from this console: closing the launcher window must not
rem deliver CTRL_CLOSE to postgres (that killed the DB at 2026-10-20 23:09).
powershell -NoProfile -ExecutionPolicy Bypass -File "%ROOT%\scripts\pg\start-detached.ps1" -PgBin "%PGBIN%" -Data "%PGDATA%" -Log "%PGLOG%"
rem pg_ctl can also fail while the server did come up; the port decides.
powershell -NoProfile -ExecutionPolicy Bypass -File "%ROOT%\scripts\pg\port-ready.ps1"
if errorlevel 1 goto failed
echo [pg] started on 127.0.0.1:55432

:pg_ready

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
