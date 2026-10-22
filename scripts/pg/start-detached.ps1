# Start portable PostgreSQL detached from the caller's console.
#
# Why detached: pg_ctl inherits the console of whoever launches it. When the
# launcher window closes, Windows sends CTRL_CLOSE to every process on that
# console and PostgreSQL dies with 0xC000013A while the gateway (its own
# hidden console) survives — the panel then fails with "connection refused"
# on 55432 (observed 2026-10-20). Start-Process -WindowStyle Hidden gives the
# server its own console.
#
# Why no "-w": pg_ctl's ready-wait has proven unreliable here — the server
# logged "ready to accept connections" and the port was listening, yet
# "pg_ctl -w start" never returned and the launcher hung for minutes
# (observed 2026-10-22: the first start-all after a stop always stalled at
# the database step; the second one worked because the server was already
# up). Readiness is decided by polling the port instead.
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
    'start'
)

# No -Wait: Start-Process -Wait has been observed to never return here even
# after pg_ctl.exe exited (2026-10-22, the launcher hung past its own 30s
# deadline with no child process left). Poll the process ourselves.
$p = Start-Process -FilePath $pgctl -ArgumentList $args -WindowStyle Hidden -PassThru
$pgDeadline = (Get-Date).AddSeconds(40)
while (-not $p.HasExited -and (Get-Date) -lt $pgDeadline) { Start-Sleep -Milliseconds 300 }
if (-not $p.HasExited) { exit 1 }
if ($p.ExitCode -ne 0) { exit $p.ExitCode }

# The server process is spawned; wait until it accepts connections.
# A real TCP connect, not Get-NetTCPConnection: that cmdlet has hung forever
# inside this detached script on this machine (2026-10-22) even though the
# port was listening.
function Test-PgReady {
    $tcp = $null
    try {
        $tcp = New-Object System.Net.Sockets.TcpClient
        $iar = $tcp.BeginConnect('127.0.0.1', 55432, $null, $null)
        $done = $iar.AsyncWaitHandle.WaitOne(1000, $false) -and $tcp.Connected
        return [bool]$done
    } catch {
        return $false
    } finally {
        if ($tcp) { $tcp.Close() }
    }
}
$deadline = (Get-Date).AddSeconds(30)
while ((Get-Date) -lt $deadline) {
    if (Test-PgReady) { exit 0 }
    Start-Sleep -Milliseconds 500
}
Write-Error "postgres did not accept connections on 55432 within 30s; see $Log"
exit 1
