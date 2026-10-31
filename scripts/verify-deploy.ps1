# Verify that what is RUNNING matches what was BUILT, and that the binary
# matches the SOURCE. Three independent facts, because each has lied before:
#
#   1) exe mtime vs newest .go source   - was the build taken in?
#   2) process StartTime vs exe mtime   - is the running process the new one?
#   3) port 13140 owner == that process - is it the gateway we are looking at?
#
# WHY (2026-11-01): the project repeatedly reported "fixed" while the running
# process predated the fix. Screenshots and endpoint behaviour were accepted as
# proof; neither can distinguish a new binary from an old one. Only these three
# process facts can.
#
# Read-only: queries and prints. Never stops, starts, or touches anything.
#
# Usage: powershell -NoProfile -File scripts\verify-deploy.ps1

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Push-Location $root
try {
    $exe = Get-Item 'bargainproxy.exe' -ErrorAction SilentlyContinue
    if (-not $exe) {
        Write-Host '[fail] bargainproxy.exe not found'
        exit 2
    }
    Write-Host ('[1] exe mtime       : ' + $exe.LastWriteTime.ToString('yyyy-MM-dd HH:mm:ss'))

    $newer = @(Get-ChildItem -Recurse -Filter *.go -Path cmd, internal, scripts -ErrorAction SilentlyContinue |
        Where-Object { $_.LastWriteTime -gt $exe.LastWriteTime })
    if ($newer.Count -gt 0) {
        Write-Host ('    [STALE] ' + $newer.Count + ' .go file(s) newer than the exe - run scripts\build.cmd')
        $newer | Select-Object -First 5 | ForEach-Object { Write-Host ('            ' + $_.FullName) }
    }
    else {
        Write-Host '    [ok] no .go source is newer than the exe'
    }

    $conn = Get-NetTCPConnection -LocalPort 13140 -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $conn) {
        Write-Host '[2] running process : [none] port 13140 is not listening - gateway is down'
        exit 1
    }
    $p = Get-Process -Id $conn.OwningProcess -ErrorAction SilentlyContinue
    if (-not $p) {
        Write-Host '[2] running process : [unknown] port owned by a pid that is gone'
        exit 1
    }
    Write-Host ('[2] running process : pid ' + $p.Id + ' started ' + $p.StartTime.ToString('yyyy-MM-dd HH:mm:ss'))

    if ($p.StartTime -gt $exe.LastWriteTime) {
        Write-Host '    [ok] process started AFTER the exe was built - the new binary is live'
    }
    else {
        Write-Host '    [STALE] process started BEFORE the exe was built - still running the OLD binary'
        Write-Host '            restart the gateway to take the build in (manual act; it serves this session)'
    }

    Write-Host ('[3] port 13140 owner: pid ' + $p.Id + ' (' + $p.ProcessName + ')')

    try {
        $r = Invoke-WebRequest -Uri 'http://127.0.0.1:13140/v1/models' -UseBasicParsing -TimeoutSec 5
        Write-Host ('[4] /v1/models      : HTTP ' + $r.StatusCode + ' (' + $r.RawContentLength + ' bytes)')
    }
    catch {
        Write-Host ('[4] /v1/models      : FAILED - ' + $_.Exception.Message)
    }
}
finally {
    Pop-Location
}
