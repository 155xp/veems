# Runs once at first logon during `veems build windows`. It installs the
# virtio drivers and guest agent, quiets the system, removes Defender, then
# reboots into veems-finish.ps1, which trims the disk and powers off.
Start-Transcript -Path C:\veems-setup.log
$ErrorActionPreference = 'Continue'
$ProgressPreference = 'SilentlyContinue'

$virtio = (Get-PSDrive -PSProvider FileSystem | Where-Object { Test-Path (Join-Path $_.Root 'virtio-win-gt-x64.msi') } | Select-Object -First 1).Root
Write-Output "virtio media: $virtio"
Start-Process msiexec.exe -Wait -ArgumentList "/i `"$($virtio)virtio-win-gt-x64.msi`" /qn /norestart ADDLOCAL=ALL"
Start-Process msiexec.exe -Wait -ArgumentList "/i `"$($virtio)guest-agent\qemu-ga-x86_64.msi`" /qn /norestart"

# Same static address user-mode networking hands out, so a restored snapshot
# has working networking the moment it is given a network.
for ($i = 0; $i -lt 90; $i++) {
  $nic = Get-NetAdapter -Physical -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($nic) { break }
  Start-Sleep 2
}
Write-Output "nic: $($nic.Name)"
Remove-NetIPAddress -InterfaceIndex $nic.ifIndex -Confirm:$false -ErrorAction SilentlyContinue
New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress 10.0.2.15 -PrefixLength 24 -DefaultGateway 10.0.2.2
Set-DnsClientServerAddress -InterfaceIndex $nic.ifIndex -ServerAddresses 10.0.2.3

# Evaluation editions must be activated online or they shut down hourly.
for ($i = 0; $i -lt 10; $i++) {
  cscript.exe //nologo C:\Windows\System32\slmgr.vbs /ato
  if ($LASTEXITCODE -eq 0) { break }
  Start-Sleep 15
}
cscript.exe //nologo C:\Windows\System32\slmgr.vbs /dli

# A calm box: nothing updates, indexes, phones home, or pops up dialogs.
Set-NetFirewallProfile -All -Enabled False
foreach ($s in 'wuauserv','UsoSvc','DiagTrack','dmwappushservice','MapsBroker','WSearch','SysMain','WerSvc','PcaSvc') {
  Stop-Service $s -Force -ErrorAction SilentlyContinue
  Set-Service $s -StartupType Disabled -ErrorAction SilentlyContinue
}
reg add "HKLM\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU" /v NoAutoUpdate /t REG_DWORD /d 1 /f
reg add "HKLM\SOFTWARE\Microsoft\Windows\Windows Error Reporting" /v DontShowUI /t REG_DWORD /d 1 /f
reg add "HKLM\SOFTWARE\Microsoft\Windows\Windows Error Reporting" /v Disabled /t REG_DWORD /d 1 /f
reg add "HKLM\SYSTEM\CurrentControlSet\Control\FileSystem" /v LongPathsEnabled /t REG_DWORD /d 1 /f
reg add "HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation" /v RealTimeIsUniversal /t REG_DWORD /d 1 /f
powercfg /hibernate off
powercfg /setactive 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c
Get-ScheduledTask -TaskPath '\Microsoft\Windows\Defrag\' -ErrorAction SilentlyContinue | Disable-ScheduledTask
Get-ScheduledTask -TaskName 'ScheduledDefrag','ProactiveScan','*Compatibility Appraiser*','Consolidator','ServerManager' -ErrorAction SilentlyContinue | Disable-ScheduledTask
if (Get-Command Set-SConfig -ErrorAction SilentlyContinue) { Set-SConfig -AutoLaunch $false }
Set-MpPreference -DisableRealtimeMonitoring $true -ErrorAction SilentlyContinue
Uninstall-WindowsFeature Windows-Defender -ErrorAction SilentlyContinue

if ({{DESKTOP}}) {
  # Log in as Administrator at every boot so a desktop is always up.
  $wl = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
  Set-ItemProperty $wl AutoAdminLogon '1'
  Set-ItemProperty $wl DefaultUserName 'Administrator'
  Set-ItemProperty $wl DefaultPassword '{{PASSWORD}}'
  Remove-ItemProperty $wl AutoLogonCount -ErrorAction SilentlyContinue
  reg add "HKLM\SOFTWARE\Microsoft\ServerManager" /v DoNotOpenServerManagerAtLogon /t REG_DWORD /d 1 /f
  reg add "HKLM\SOFTWARE\Policies\Microsoft\Windows\Control Panel\Desktop" /v ScreenSaveActive /t REG_SZ /d 0 /f
  reg add "HKLM\SOFTWARE\Policies\Microsoft\Windows\Personalization" /v NoLockScreen /t REG_DWORD /d 1 /f
  reg add "HKLM\SOFTWARE\Policies\Microsoft\Windows NT\Reliability" /v ShutdownReasonOn /t REG_DWORD /d 0 /f
  powercfg /change monitor-timeout-ac 0
}
New-Item -ItemType Directory -Force C:\work, C:\veems | Out-Null
$finish = @'
Start-Transcript -Path C:\veems-finish.log
Start-Sleep 60
Unregister-ScheduledTask -TaskName veems-finish -Confirm:$false
Remove-Item -Recurse -Force C:\Windows\Temp\*, C:\Windows\SoftwareDistribution\Download\* -ErrorAction SilentlyContinue
Optimize-Volume -DriveLetter C -ReTrim -ErrorAction SilentlyContinue
Stop-Transcript
Stop-Computer -Force
'@
Set-Content -Path C:\veems\finish.ps1 -Value $finish
$action = New-ScheduledTaskAction -Execute powershell.exe -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\veems\finish.ps1'
Register-ScheduledTask -TaskName veems-finish -Action $action -Trigger (New-ScheduledTaskTrigger -AtStartup) -User SYSTEM -RunLevel Highest -Force
Stop-Transcript
Restart-Computer -Force
