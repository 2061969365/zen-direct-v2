# 可见窗口模式：重启网关 → 打印健康状态 → 等你按键再关（网关继续后台跑）
# 桌面快捷方式指向这个脚本，双击就会弹出一个 PowerShell 窗口。
$ErrorActionPreference = 'Continue'
$home_ = $PSScriptRoot
if (-not $home_) { $home_ = Split-Path -Parent $MyInvocation.MyCommand.Path }

Write-Host ''
Write-Host '========================================' -ForegroundColor Cyan
Write-Host '  zen-direct v2  (native TLS, no bun)' -ForegroundColor Cyan
Write-Host '========================================' -ForegroundColor Cyan
Write-Host ''

& (Join-Path $home_ 'run-gateway.ps1')
Start-Sleep -Milliseconds 800

try {
  $h = curl.exe -s -m 5 'http://127.0.0.1:8081/health'
  Write-Host ''
  Write-Host 'health:' -ForegroundColor Green
  Write-Host "  $h"
} catch {
  Write-Host "健康检查失败：$($_.Exception.Message)" -ForegroundColor Red
}

$exe = Join-Path $home_ 'zen-direct.exe'
$proc = Get-CimInstance Win32_Process -Filter "Name='zen-direct.exe'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -eq $exe }
if ($proc) {
  Write-Host ''
  Write-Host ("  PID {0}   端口 8081   日志 {1}\logs" -f $proc.ProcessId, $home_) -ForegroundColor DarkGray
}

Write-Host ''
Write-Host '网关在后台继续运行；按任意键关闭本窗口。' -ForegroundColor Yellow
try {
  $null = $Host.UI.RawUI.ReadKey('NoEcho,IncludeKeyDown')
} catch {
  # 没有交互终端（例如输入被重定向）时退化成 10 秒后自动关闭
  Start-Sleep -Seconds 10
}
