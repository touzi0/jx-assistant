# 开发用桌面版：构建到仓库根的 AI数字助理.exe
# 双击即桌面窗口（构建期注入 desktopDefault=1 + GUI 子系统无 CMD 窗口，
# 日志改落 data\desktop.log）；数据/config 照旧落仓库目录，与
# jx-assistant.exe（浏览器形态、控制台）互不干扰。
# 用法：powershell -NoProfile -ExecutionPolicy Bypass -File 打包\开发桌面版.ps1
#       或直接双击根目录「编译并启动桌面版.bat」（调用的就是本脚本）
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot

# 1) 先关正在运行的桌面实例（两类坑一次清掉）：
#    a) exe 被运行中的进程占用，go build 写不进去；
#    b) 单实例互斥体（所有桌面 exe 共用一名）会让新 exe「启动即退出、
#       唤起旧窗口」——你以为是新构建，实际还是旧进程在跑。
#    只关桌面形态，不动 jx-assistant.exe（网页形态开发实例）。
#    exe 已切新名（AI数字助理，2026-11-19）：旧名（坚祥数字助理GO，热更
#    迁移前的存量机器）与新名都可能在本机跑，都列上。
$desktopProcs = @(Get-CimInstance Win32_Process -Filter "Name='AI数字助理.exe' or Name='坚祥数字助理GO.exe' or Name='坚祥数字助理GO-右键菜单版.exe'" -ErrorAction SilentlyContinue)
if ($desktopProcs.Count -gt 0) {
  foreach ($p in $desktopProcs) {
    Write-Host ("关闭旧桌面实例 pid=" + $p.ProcessId)
    try { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop } catch { }
  }
  Start-Sleep -Seconds 2
}

# 2) 源码状态提示（不阻断）：工作区有未提交改动时构建仍可进行，但若
#    另一场对话正在改代码，此刻编译会把中间态一起编进去（「半截构建」
#    的根源不是脚本失败，而是源码树本身处于混合状态）。
try {
  $dirty = @(git -C $root status --porcelain 2>$null)
  if ($dirty.Count -gt 0) {
    Write-Host ("[提示] 工作区有 " + $dirty.Count + " 处未提交改动。若有另一场对话正在改代码，" +
      "请等它收口（提交）后再构建，否则产物会包含中间态。") -ForegroundColor Yellow
  }
} catch { }

# 3) 编译（失败即抛出，调用方按退出码拒绝启动旧 exe）
# 先把组件里的独立语音输入法编出来（独立 Go 模块，主程序 build 不带它）
& (Join-Path $PSScriptRoot "编译语音输入法.ps1")
if ($LASTEXITCODE -ne 0) { throw "语音输入法编译失败" }

go build -trimpath -ldflags "-s -w -H=windowsgui -X main.desktopDefault=1 -X main.appVersion=20991231-1" -o (Join-Path $root "AI数字助理.exe") $root  # appVersion 注入未来版本号：开发版永远比服务器新，不弹自动更新
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
Write-Host "完成：$root\AI数字助理.exe（开发桌面版，gitignore 的本地产物）"
