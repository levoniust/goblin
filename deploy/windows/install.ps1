# Install (or update) the Goblin host on Windows. Run in PowerShell from the folder that holds goblin.exe and
# goblinw.exe (the same program built without a console window: the tasks and the shortcut use it, so nothing of
# Goblin's ever opens a window on the desktop; goblin.exe is for typing commands):
#   powershell -ExecutionPolicy Bypass -File install.ps1
# Two Scheduled Tasks start at logon, hidden, and restart on failure:
#   "Goblin keeper" holds the chats (ConPTY); "Goblin host" is the network + API. Updating the host never ends chats.
# Then run `C:\AI\goblin\goblin.exe init` (choose your PIN) and `goblin pair`, or join an existing network with
# `goblin join HOST:8200` and approve it on your phone. Settings: %APPDATA%\goblin\goblin.yaml (`goblin config check`).
# The "Goblin" shortcut opens this computer's page (`goblin ui`).
$ErrorActionPreference = 'Stop'
$dir = 'C:\AI\goblin'
New-Item -ItemType Directory -Force $dir | Out-Null
$here = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
$exe = Join-Path $dir 'goblin.exe'
$exew = Join-Path $dir 'goblinw.exe'
# only the host stops: the keeper (and your chats) keep running on the old file, which Windows lets us rename
Stop-ScheduledTask -TaskName 'Goblin host' -ErrorAction SilentlyContinue
Start-Sleep 1
foreach ($name in 'goblin.exe', 'goblinw.exe') {
  $src = Join-Path $here $name
  $dst = Join-Path $dir $name
  if (-not (Test-Path $src) -or (Resolve-Path $src).Path -eq $dst) { continue }
  if (Test-Path $dst) {
    $base = [IO.Path]::GetFileNameWithoutExtension($name)
    $old = Join-Path $dir "$base-old.exe"
    Remove-Item $old -Force -ErrorAction SilentlyContinue   # still in use by an even older keeper: keep it
    if (-not (Test-Path $old)) { Rename-Item $dst "$base-old.exe" } else { Rename-Item $dst ("$base-old-{0}.exe" -f (Get-Date -Format yyyyMMddHHmmss)) }
  }
  Copy-Item $src $dst -Force
}
if (-not (Test-Path $exew)) { $exew = $exe }   # an install without goblinw.exe still works (with a console window)
$set = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -Hidden -StartWhenAvailable -MultipleInstances IgnoreNew
$trig = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
# logon start only (2026-11-17: no 5-minute repeat); restart-on-failure still covers crashes
$prin = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -TaskName 'Goblin keeper' -Action (New-ScheduledTaskAction -Execute $exew -Argument 'keeper' -WorkingDirectory $dir) `
  -Trigger $trig -Settings $set -Principal $prin -Force | Out-Null
Register-ScheduledTask -TaskName 'Goblin host' -Action (New-ScheduledTaskAction -Execute $exew -Argument 'run' -WorkingDirectory $dir) `
  -Trigger $trig -Settings $set -Principal $prin -Force | Out-Null
# firewall: the API (TCP 8200) and LAN discovery (UDP 47820) on every network profile: Windows calls a phone hotspot
# or hotel Wi-Fi "Public", and Goblin must work there (the design rules). Every request is signed and TLS-pinned.
foreach ($r in @(@{n='Goblin host (TCP 8200)'; p='TCP'; port=8200}, @{n='Goblin discovery (UDP 47820)'; p='UDP'; port=47820})) {
  try {
    Get-NetFirewallRule -DisplayName $r.n -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    New-NetFirewallRule -DisplayName $r.n -Direction Inbound -Protocol $r.p -LocalPort $r.port -Profile Any -Action Allow | Out-Null
  } catch { Write-Warning "firewall rule '$($r.n)' needs an admin PowerShell: $_" }
}
# "Goblin" shortcut (Start menu + desktop): opens this computer's Goblin page (goblin ui) in the browser
$wsh = New-Object -ComObject WScript.Shell
foreach ($lnkDir in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
  try {
    $lnk = $wsh.CreateShortcut((Join-Path $lnkDir 'Goblin.lnk'))
    $lnk.TargetPath = $exew; $lnk.Arguments = 'ui'; $lnk.WorkingDirectory = $dir; $lnk.WindowStyle = 7
    $lnk.IconLocation = "$exe,0"; $lnk.Description = "This computer's Goblin page"
    $lnk.Save()
  } catch { Write-Warning "shortcut in $lnkDir`: $_" }
}
if ((Get-ScheduledTask -TaskName 'Goblin keeper').State -ne 'Running') { Start-ScheduledTask -TaskName 'Goblin keeper'; Start-Sleep 2 }
Start-ScheduledTask -TaskName 'Goblin host'
Start-Sleep 3
& $exe status
