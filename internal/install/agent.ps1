<#
Lumen agent installer for Windows. Run in an elevated (Administrator) PowerShell:

  & ([scriptblock]::Create((irm __LUMEN_URL__/install/agent.ps1))) -Key YOUR_API_KEY

Options:  -LogPath 'C:\logs\*.log' (repeatable)   -Url <override>   -Uninstall
#>
param(
  [string]$Key = $env:LUMEN_KEY,
  [string]$Url = "__LUMEN_URL__",
  [string[]]$LogPath = @(),
  [switch]$Uninstall
)
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$Task = 'LumenAgent'
$BinDir = Join-Path $env:ProgramFiles 'Lumen'
$ConfDir = Join-Path $env:ProgramData 'Lumen'
$Exe = Join-Path $BinDir 'lumen-agent.exe'
$Conf = Join-Path $ConfDir 'agent.json'

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { throw 'Run this in an elevated PowerShell (Run as administrator).' }

if ($Uninstall) {
  Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName $Task -Confirm:$false -ErrorAction SilentlyContinue
  Get-Process lumen-agent -ErrorAction SilentlyContinue | Stop-Process -Force
  Remove-Item $BinDir, $ConfDir -Recurse -Force -ErrorAction SilentlyContinue
  Write-Host 'Lumen agent removed.'; return
}

if (-not $Key) { throw 'An API key is required: -Key YOUR_API_KEY' }
if ($Key -notmatch '^[A-Za-z0-9_.-]+$') { throw 'The API key contains unexpected characters.' }
if ($Url -notmatch '^https?://[A-Za-z0-9.:-]+$') { throw "Invalid Lumen URL: $Url" }

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$name = "lumen-agent-windows-$arch.exe"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading $name from $Url ..."
  Invoke-WebRequest -UseBasicParsing "$Url/download/$name" -OutFile (Join-Path $tmp $name)
  Invoke-WebRequest -UseBasicParsing "$Url/download/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
  $want = ((Get-Content (Join-Path $tmp 'SHA256SUMS')) | Where-Object { $_ -match " \*?$([regex]::Escape($name))$" } | Select-Object -First 1) -split '\s+' | Select-Object -First 1
  $have = (Get-FileHash (Join-Path $tmp $name) -Algorithm SHA256).Hash.ToLower()
  if (-not $want -or $want.ToLower() -ne $have) { throw 'Checksum mismatch: the download is corrupt, refusing to install.' }

  Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
  Get-Process lumen-agent -ErrorAction SilentlyContinue | Stop-Process -Force
  New-Item -ItemType Directory -Force -Path $BinDir, $ConfDir | Out-Null
  Copy-Item (Join-Path $tmp $name) $Exe -Force
} finally { Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue }

$logs = @()
foreach ($p in $LogPath) { $logs += @{ paths = @($p); service = 'system-logs'; format = 'text' } }
@{ url = $Url; api_key = $Key; interval_seconds = 15; host_metrics = $true; self_metrics = $true; logs = $logs } |
  ConvertTo-Json -Depth 5 | Set-Content -Path $Conf -Encoding ASCII
# the config holds the API key: only SYSTEM and Administrators may read it
& icacls.exe $Conf /inheritance:r /grant:r 'SYSTEM:(F)' 'Administrators:(F)' | Out-Null

$action = New-ScheduledTaskAction -Execute $Exe -Argument "-config `"$Conf`""
$trigger = New-ScheduledTaskTrigger -AtStartup
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $Task -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $Task
Start-Sleep -Seconds 3
if (Get-Process lumen-agent -ErrorAction SilentlyContinue) {
  Write-Host 'Lumen agent installed and running. Data should appear in Lumen within a minute.'
  Write-Host 'Remove: re-run this script with -Uninstall'
} else { throw 'The agent did not start. Check Task Scheduler > LumenAgent > History.' }
