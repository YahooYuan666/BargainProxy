# Start portable PostgreSQL so that the launcher window can close.
#
# The two Windows traps, both observed in the field:
#
# 1) Console inheritance kills the database. A process started from our console
#    keeps that console attached; when the launcher window closes, Windows sends
#    CTRL_CLOSE to everyone on it and PostgreSQL dies with 0xC000013A while the
#    gateway (its own hidden console) survives. The panel then fails with
#    "connection refused" on 55432 (2026-10-20).
#
# 2) pg_ctl's cmd wrapper keeps the window open. On Windows "pg_ctl start" runs
#    the server through an intermediate "cmd.exe /D /C postgres.exe ...". That
#    wrapper inherits the caller's console and lives as long as the server does,
#    so the launcher window never closes even after start-all.cmd printed its
#    last line (2026-10-22: a cmd.exe whose parent was long gone still held
#    postgres.exe, and the console stayed open indefinitely). Starting
#    postgres.exe directly is not an option either — it refuses to run as
#    administrator ("Execution of PostgreSQL by a user with administrative
#    permissions is not permitted"), which is exactly what pg_ctl's privilege
#    drop handles for us.
#
# So pg_ctl stays, and the whole invocation is created through WMI
# (Win32_Process.Create): that process has no parent console at all, so neither
# it nor the cmd wrapper it spawns can hold ours. The call returns as soon as
# the process is created (~130ms measured); readiness is decided by polling the
# port, never by pg_ctl's own -w ready-wait (unreliable here: "ready to accept
# connections" logged and the port listening, yet the wait never returned).
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

# Quote each path for the command line WMI will build.
$cmdLine = '"{0}" -D "{1}" -l "{2}" -o "-p 55432 -c listen_addresses=127.0.0.1" start' -f $pgctl, $Data, $Log

try {
    $r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cmdLine }
} catch {
    Write-Error "failed to create postgres starter: $_"
    exit 1
}
if ($r.ReturnValue -ne 0) {
    # 9 = path not found, 21 = invalid parameter; report the code, it is the
    # only diagnostic Win32_Process.Create gives.
    Write-Error "Win32_Process.Create failed with code $($r.ReturnValue); see $Log"
    exit 1
}

# A real TCP connect, not Get-NetTCPConnection: that cmdlet has hung forever
# inside this detached script on this machine (2026-10-22) even though the port
# was listening.
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
