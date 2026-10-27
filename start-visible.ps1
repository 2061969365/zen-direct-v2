# 可见模式：重启网关 + 在窗口里打印健康状态，15 秒后自动关闭（网关继续后台运行）
$ErrorActionPreference = 'Continue'
$home_ = $PSScriptRoot
if (-not $home_) { $home_ = Split-Path -Parent $MyInvocation.MyCommand.Path }

Write-Host ''
Write-Host '=== zen-direct v2 ===' -ForegroundColor Cyan
& (Join-Path $home_ 'run-gateway.ps1')
Start-Sleep -Milliseconds 800

try {
  $h = curl.exe -s -m 5 'http://127.0.0.1:8081/health'
  Write-Host ''
  Write-Host 'health:' -ForegroundColor Green
  Write-Host $h
} catch {
  Write-Host "健康检查失败：$($_.Exception.Message)" -ForegroundColor Red
  Write-Host "日志：$home_\logs"
}

$proc = Get-CimInstance Win32_Process -Filter "Name='zen-direct.exe'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -eq (Join-Path $home_ 'zen-direct.exe') }
if ($proc) {
  Write-Host ''
  Write-Host "PID $($proc.ProcessId)  端口 8081  日志 $home_\logs" -ForegroundColor DarkGray
}

Write-Host ''
Write-Host '15 秒后关闭本窗口；网关在后台继续运行。' -ForegroundColor DarkGray
Start-Sleep -Seconds 15
