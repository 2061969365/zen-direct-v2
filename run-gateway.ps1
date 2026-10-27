# zen-direct v2 常驻启动器（原生传输，不需要 bun）
# 双击、计划任务、启动目录快捷方式都可以调；重复运行会先收掉自己那一个进程再起。
$ErrorActionPreference = 'Stop'

$home_ = $PSScriptRoot
if (-not $home_) { $home_ = Split-Path -Parent $MyInvocation.MyCommand.Path }

# ===================== CONFIG =====================
$Port        = 8081
$Concurrency = '16'
$LogMaxMB    = '200'
$LogKeepDays = 7
$ZenBase     = 'https://opencode.ai/inference/v1'
$FreshPerTurn = '0'      # 0=一对话一号（撞 429 才换） 1=每轮新号
# ==================================================

$StateDir = Join-Path $home_ 'state'
$LogDir   = Join-Path $home_ 'logs'
New-Item -ItemType Directory -Force -Path $StateDir, $LogDir | Out-Null

$exe = Join-Path $home_ 'zen-direct.exe'
if (-not (Test-Path $exe)) { throw "missing $exe" }

# 只收掉 v2 自己的进程（按 exe 路径匹配），不误伤老网关
Get-CimInstance Win32_Process -Filter "Name='zen-direct.exe'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -eq $exe } |
  ForEach-Object { try { Stop-Process -Id $_.ProcessId -Force } catch {} }
Start-Sleep 1

# 会话池 / 真号名单：首次启动时沿用老运行目录的最新状态
$pool    = Join-Path $StateDir 'shim_sessions.json'
$genuine = Join-Path $StateDir 'genuine_sessions.txt'
$oldDir  = 'F:\worker\zen-direct'
if (-not (Test-Path $pool)    -and (Test-Path (Join-Path $oldDir 'shim_sessions.json')))    { Copy-Item (Join-Path $oldDir 'shim_sessions.json')    $pool }
if (-not (Test-Path $genuine) -and (Test-Path (Join-Path $oldDir 'genuine_sessions.txt'))) { Copy-Item (Join-Path $oldDir 'genuine_sessions.txt') $genuine }

# 日志保留
Get-ChildItem (Join-Path $LogDir 'zen-direct-*.err.log*') -ErrorAction SilentlyContinue |
  Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-$LogKeepDays) } |
  ForEach-Object { Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue }

$log = Join-Path $LogDir ('zen-direct-{0}.err.log' -f (Get-Date -Format 'yyyyMMdd'))

$env:PORT                  = "$Port"
$env:DIRECT_DEBUG          = '1'
$env:DIRECT_DIAG           = '1'
$env:DIRECT_CONCURRENCY    = $Concurrency
$env:LOG_FILE              = $log
$env:LOG_MAX_MB            = "$LogMaxMB"
$env:OPENCODE_ZEN_BASE     = $ZenBase
$env:DIRECT_FRESH_PER_TURN = $FreshPerTurn
$env:SHIM_SESSION_FILE     = $pool
$env:GENUINE_SESSION_FILE  = $genuine
# 原生传输：uTLS 复刻 Bun ClientHello + fetch 头序；不拉起 bun sidecar
$env:ZEN_TLS               = 'bun'
$env:DIRECT_SIDECAR        = 'off'

Start-Process -FilePath $exe -WindowStyle Hidden
Start-Sleep 2

try {
  $h = curl.exe -s -m 5 "http://127.0.0.1:$Port/health"
  Write-Host "health: $h"
} catch {
  Write-Host "start failed, see $log"
}
