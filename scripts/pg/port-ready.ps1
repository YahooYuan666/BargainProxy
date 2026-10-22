# Exit 0 when postgres accepts TCP connections on 55432, else 1.
# A plain TcpClient: Get-NetTCPConnection has hung inside the detached
# launcher on this machine (2026-10-22) even while the port was listening.
$tcp = $null
try {
    $tcp = New-Object System.Net.Sockets.TcpClient
    $iar = $tcp.BeginConnect('127.0.0.1', 55432, $null, $null)
    $ok = $iar.AsyncWaitHandle.WaitOne(2000, $false) -and $tcp.Connected
} catch {
    $ok = $false
} finally {
    if ($tcp) { $tcp.Close() }
}
if ($ok) { exit 0 } else { exit 1 }
