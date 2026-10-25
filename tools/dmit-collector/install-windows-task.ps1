param(
  [string]$TaskName = "Komari DMIT Official Traffic Collector",
  [int]$IntervalMinutes = 30
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$Node = (Get-Command node).Source
if (-not $Node) {
  throw "Node.js not found. Please install Node.js LTS first."
}

$Action = New-ScheduledTaskAction -Execute $Node -Argument "`"$Root\collector.js`" --once" -WorkingDirectory $Root
$Trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) -RepetitionInterval (New-TimeSpan -Minutes $IntervalMinutes)
$Principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel LeastPrivilege

Register-ScheduledTask -TaskName $TaskName -Action $Action -Trigger $Trigger -Principal $Principal -Force | Out-Null
Write-Host "Installed task: $TaskName, every $IntervalMinutes minutes"
