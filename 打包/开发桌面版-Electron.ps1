# 开发用 Electron 桌面壳：构建 Go 引擎 + 拉起 jx-desktop（npm start）
# Go 侧产物是控制台版 jx-assistant.exe（sidecar 由 Electron 拉起，stdio 被壳
# 接管写 userData\logs\shell.log，排查比旧壳的 data\desktop.log 只多不少）；
# 数据/config 照旧落仓库目录（开发目录有 go.mod，-electron 不搬迁 cwd）。
# 用法：powershell -NoProfile -ExecutionPolicy Bypass -File 打包\开发桌面版-Electron.ps1
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$sh = Join-Path (Split-Path -Parent $root) "jx-desktop"

# 1) 关正在运行的开发实例：exe 被 Go 进程占用时 go build 写不进去。
#    注意按名杀 jx-assistant.exe 会连带关掉「网页形态开发实例」（浏览器里
#    刷新前的服务端）——Electron 开发流程里它们本就共用这个产物，重建即重来。
$goProcs = @(Get-CimInstance Win32_Process -Filter "Name='jx-assistant.exe'" -ErrorAction SilentlyContinue)
if ($goProcs.Count -gt 0) {
  foreach ($p in $goProcs) {
    Write-Host ("关闭旧引擎实例 pid=" + $p.ProcessId)
    try { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop } catch { }
  }
  Start-Sleep -Seconds 1
}

# 2) 源码状态提示（不阻断，同旧脚本口径：半截构建的根源在源码树混合状态）
try {
  $dirty = @(git -C $root status --porcelain 2>$null)
  if ($dirty.Count -gt 0) {
    Write-Host ("[提示] 工作区有 " + $dirty.Count + " 处未提交改动。若有另一场对话正在改代码，" +
      "请等它收口（提交）后再构建，否则产物会包含中间态。") -ForegroundColor Yellow
  }
} catch { }

# 3) 编译 Go 引擎（控制台版；不注入 desktopDefault，形态由 Electron 的
#    -electron 参数决定）
go build -trimpath -ldflags "-s -w -X main.appVersion=20991231-1" -o (Join-Path $root "jx-assistant.exe") $root  # appVersion 注入未来版本号：开发版永远比服务器新，不弹自动更新
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
Write-Host "完成：$root\jx-assistant.exe（sidecar 引擎）"

# 4) Electron 壳依赖（首次安装；node_modules 在 jx-desktop\.gitignore 里）。
#    npm 的 allow-scripts 策略可能拦掉 electron 包的 postinstall（二进制下载），
#    装完检查 dist\electron.exe，缺了就走国内镜像补跑安装脚本。
if (-not (Test-Path (Join-Path $sh "node_modules\electron"))) {
  Write-Host "安装 Electron 壳依赖（首次较慢）..."
  Push-Location $sh
  try { npm install; if ($LASTEXITCODE -ne 0) { throw "npm install 失败" } } finally { Pop-Location }
}
if (-not (Test-Path (Join-Path $sh "node_modules\electron\dist\electron.exe"))) {
  Write-Host "Electron 二进制缺失（postinstall 被拦或网络失败），经 npmmirror 补装..."
  Push-Location $sh
  try {
    $env:ELECTRON_MIRROR = "https://npmmirror.com/mirrors/electron/"
    node node_modules\electron\install.js
    if ($LASTEXITCODE -ne 0) { throw "Electron 二进制补装失败" }
  } finally { Pop-Location }
}

# 5) 启动（阻塞直到窗口关闭；Ctrl+C 结束会连带 taskkill 引擎树）
$env:JX_GO_EXE = Join-Path $root "jx-assistant.exe"
$env:JX_SKIP_MIGRATIONS = "1"   # 开发实例兜底闸：启动时整体跳过迁移（不碰技能目录与账本；正式版不设此变量，行为不变）
Push-Location $sh
try { npm start; if ($LASTEXITCODE -ne 0) { throw "npm start 失败（退出码 $LASTEXITCODE）" } } finally { Pop-Location }
