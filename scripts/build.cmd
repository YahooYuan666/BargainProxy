@echo off
rem Build the gateway binary, always.
rem
rem WHY THIS EXISTS (2026-11-01): start-all.cmd used to compile only when
rem bargainproxy.exe was MISSING. Editing Go code and re-running start-all
rem therefore restarted the OLD binary, silently — the change was never
rem compiled. That is the root cause behind the long history of "the fix is in
rem the source but the behaviour did not change" (67 archived
rem bargainproxy.exe.bak-* files are the scar tissue), and behind reports
rem claiming a fix was live when the running process predated it.
rem
rem This script is the one place a build happens. It always builds, then PROVES
rem the result is newer than every Go source file. It NEVER stops or starts
rem anything — restarting the gateway is a separate, explicit act, because the
rem gateway serves the LLM traffic that runs this very session.
setlocal
set "ROOT=%~dp0.."
pushd "%ROOT%"

where go >nul 2>nul
if errorlevel 1 (
  echo [error] Go not found in PATH. Install Go, or build elsewhere and copy bargainproxy.exe in.
  popd
  endlocal
  exit /b 1
)

echo [build] go build ./cmd/bargainproxy
go build -o bargainproxy.exe ./cmd/bargainproxy
if errorlevel 1 (
  echo [error] build failed — the existing bargainproxy.exe was NOT replaced
  popd
  endlocal
  exit /b 1
)

rem Prove the binary is newer than every Go source file. Without this check a
rem stale binary is indistinguishable from a fresh one, which is exactly the
rem trap this script closes.
powershell -NoProfile -Command ^
  "$exe = Get-Item 'bargainproxy.exe';" ^
  "$newer = Get-ChildItem -Recurse -Filter *.go -Path cmd,internal,scripts | Where-Object { $_.LastWriteTime -gt $exe.LastWriteTime };" ^
  "if ($newer) { Write-Host '[warn] these sources are NEWER than the binary (build did not take them in):'; $newer | ForEach-Object { Write-Host ('       ' + $_.FullName) }; exit 1 };" ^
  "Write-Host ('[ok] bargainproxy.exe rebuilt ' + $exe.LastWriteTime.ToString('yyyy-MM-dd HH:mm:ss') + ' — newer than every .go source')"
if errorlevel 1 goto stale

popd
endlocal
exit /b 0

:stale
popd
endlocal
exit /b 1
