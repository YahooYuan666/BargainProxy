# Start portable PostgreSQL so the launcher window can close.
#
# Three Windows traps, all observed in the field:
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
#    last line (2026-10-22).
#
# 3) A pseudoconsole counts as a console too. Creating the wrapper through WMI
#    (Win32_Process.Create) only avoids a classic console. Launched from Windows
#    Terminal, the pseudoconsole is still inherited: the window stays open, and
#    closing it kills the database (observed 2026-10-22 19:24 and 19:25 — the
#    background worker died with 0xC000013A the moment the window was closed,
#    and the next boot had to replay a crash recovery). Starting postgres.exe
#    directly is not an option either: it refuses to run as administrator, which
#    is exactly the check pg_ctl's privilege drop handles for us.
#
# The one flag that breaks the chain is DETACHED_PROCESS, and it has to be set
# on pg_ctl's own parent. start-detached-helper.exe is that parent: a few lines
# of Go that create pg_ctl with the flag and then wait for it. pg_ctl (without
# -w) exits as soon as the server process exists, so the helper exits too.
param(
    [Parameter(Mandatory = $true)][string]$PgBin,
    [Parameter(Mandatory = $true)][string]$Data,
    [Parameter(Mandatory = $true)][string]$Log
)

$pgctl = Join-Path $PgBin 'pg_ctl.exe'
$helper = Join-Path $PSScriptRoot 'start-detached-helper.exe'
foreach ($need in @($pgctl, $helper)) {
    if (-not (Test-Path $need)) {
        Write-Error "required file missing: $need"
        exit 1
    }
}

# -o takes ONE argument containing spaces, so it is handed to the helper as a
# single argv entry; the helper re-quotes it for pg_ctl.
& $helper $pgctl '-D' $Data '-l' $Log '-o' '-p 55432 -c listen_addresses=127.0.0.1' start
if ($LASTEXITCODE -ne 0) {
    Write-Error "pg_ctl start failed (exit $LASTEXITCODE); see $Log"
    exit 1
}

# A real TCP connect, not Get-NetTCPConnection: that cmdlet has hung forever
# inside this script on this machine (2026-10-22) even though the port was
# listening. The port opening is not the same as the server accepting queries
# (crash recovery still runs), so this only proves the process came up; the
# gateway retries the "starting up" state on its own.
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
