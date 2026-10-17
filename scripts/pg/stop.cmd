@echo off
rem Stop the project-local portable PostgreSQL.
setlocal
set "ROOT=%~dp0..\.."
pushd "%ROOT%"
"%ROOT%\.tools\postgres\bin\pg_ctl.exe" -D "%ROOT%\.tools\pgdata" stop
popd
