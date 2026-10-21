# Start portable PostgreSQL detached from the caller's console.
#
# Why: pg_ctl inherits the console of the script that launches it. When
# start-all.cmd finishes and the window closes, Windows sends CTRL_CLOSE to
# every process attached to that console — PostgreSQL then exits with
# 0xC000013A (STATUS_CONTROL_C_EXIT) while the gateway (started via
# Start-Process -WindowStyle Hidden) survives. The panel then fails with
# "dial tcp 127.0.0.1:55432: actively refused" (observed 2026-10-20).
#
# Starting pg_ctl through Start-Process -WindowStyle Hidden gives the server
# its own (hidden) console, so the caller's window lifecycle no longer matters.
param(
    [Parameter(Mandatory = $true)][string]$PgBin,
    [Parameter(Mandatory = $true)][string]$Data,
    [Parameter(Mandatory = $true)][string]$Log
)

$pgctl = Join-Path $PgBin 'pg_ctl.exe'
if (-not (Test-Path $pgctl)) {
    Write-Error "pg_ctl not found: $pgctl"
    exit 1
}

# Quoting matters: -D/-l take paths, -o takes ONE argument containing spaces.
$args = @(
    '-D', "`"$Data`"",
    '-l', "`"$Log`"",
    '-o', '"-p 55432 -c listen_addresses=127.0.0.1"',
    '-w', 'start'
)

$p = Start-Process -FilePath $pgctl -ArgumentList $args -WindowStyle Hidden -Wait -PassThru
exit $p.ExitCode
