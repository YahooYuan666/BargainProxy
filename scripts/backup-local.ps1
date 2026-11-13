# Backup the LOCAL-ONLY working assets that git deliberately does not track.
#
# WHY THIS EXISTS (2026-11-09): .scratch/bargainproxy/issues/ (199 ticket
# files, BP-01..BP-294) was deleted during a pre-migration disk cleanup — the
# user reasonably judged ".scratch" non-essential because git never showed it.
# But .gitignore excludes it ON PURPOSE (internal process docs must not ship
# with the public repo), so those files exist ONLY on this disk. Deleting them
# destroys the approval-binding artifacts workflow-state.md points at.
#
# The answer is an out-of-tree archive, not adding them to git:
#   - source of truth stays local (git stays clean for publishing)
#   - a dated zip lands outside the project (survives cleanup + migration)
#
# Scope: research/ + the process docs + the ticket directory. Explicitly
# EXCLUDES transient payloads (py-install, agdump, upscan caches) that only
# inflate the archive.
#
# Usage:  scripts\backup-local.cmd [destination-dir]
#         default destination: D:\Backup\BargainProxy
[CmdletBinding()]
param(
    [string]$Destination = 'D:\Backup\BargainProxy'
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$zipPath = Join-Path $Destination ("bargainproxy-local-$stamp.zip")

# --- collect -------------------------------------------------------------
$docFiles = @(
    'workflow-state.md',
    'execution-report.md',
    'review-report.md',
    'user-acceptance.md',
    'execution-handoff.md'
)

$stage = Join-Path $env:TEMP ("bp-backup-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $stage -Force | Out-Null

$copied = 0
try {
    foreach ($f in $docFiles) {
        $src = Join-Path $root $f
        if (Test-Path $src) {
            Copy-Item $src (Join-Path $stage $f)
            $copied++
        }
    }

    $research = Join-Path $root 'research'
    if (Test-Path $research) {
        Copy-Item $research (Join-Path $stage 'research') -Recurse
    }

    # The ticket directory: the ONLY irreplaceable part. Keep the .scratch
    # parent so restored paths match what workflow-state.md references.
    $tickets = Join-Path $root '.scratch\bargainproxy'
    if (Test-Path $tickets) {
        $stageScratch = Join-Path $stage '.scratch'
        New-Item -ItemType Directory -Path $stageScratch -Force | Out-Null
        Copy-Item $tickets (Join-Path $stageScratch 'bargainproxy') -Recurse
    } else {
        Write-Warning "ticket directory not found: $tickets"
    }

    $fileCount = (Get-ChildItem $stage -Recurse -File | Measure-Object).Count

    # --- write -----------------------------------------------------------
    if (-not (Test-Path $Destination)) {
        New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    }
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zipPath -CompressionLevel Optimal -Force

    $z = Get-Item $zipPath
    Write-Host ("[ok] {0}  {1:N1} KB  {2} files" -f $z.Name, ($z.Length / 1KB), $fileCount)

    # --- prune -----------------------------------------------------------
    $old = Get-ChildItem $Destination -Filter 'bargainproxy-local-*.zip' |
        Sort-Object LastWriteTime -Descending | Select-Object -Skip 10
    if ($old) {
        $old | Remove-Item -Force
        Write-Host ("[prune] removed {0} old archive(s)" -f $old.Count)
    }

    Write-Host '[done] run this before any disk cleanup or machine migration.'
} finally {
    if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
}
