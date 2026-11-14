# 开发桌面版·录屏演示（不被安装包杀掉的开发实例）
# 目的：录制「安装版安装过程 + 充值使用演示」时，开发实例以 AI数字助理.exe
#   之名运行在独立目录。安装包（打包\安装包.iss 的 TaskKillApp）按**镜像名**
#   taskkill：当前正式名 坚祥数字助理GO.exe（+迁移段旧名、MD查看器.exe），
#   名单里没有 AI数字助理.exe → 录屏中途跑安装包，开发实例照常存活。
# ★ 撞名预警（exe 名切换期）：正式版按《exe名切换-AI数字助理-一周后操作清单.md》
#   改名 AI数字助理.exe 后，正式安装包的 taskkill 名单将含 AI数字助理.exe，
#   本开发版会被安装包杀掉——届时把下方 $ExeName 改成 'AI数字助理-开发.exe'
#   （仅此一处；配置名/互斥体/联接不受影响），并把两个入口 .bat 一并改名。
# 产物目录 D:\智能体工作区\数字助理GO\dev-app（仓库外、与正式安装目录分开）。
#   组件/内置组件 用目录联接（junction）指回仓库根：桌面版把 cwd 锚到 exe 旁、
#   按 exe 旁相对路径发现这两处（components.go / mcpservers_builtin.go），没有
#   联接功能就缺一块；技能目录走 config 的 skillsDir 显式指定，不依赖联接。
# 两种数据模式（-DataMode）：
#   isolated（默认，安全）：data/workspace 落 dev-app\data、dev-app\工作区，
#     互斥体 JX-Desktop-DevApp-Isolated、随机端口——与正式实例/仓库数据互不相干，
#     可与正式版同时开。首启沿用仓库根 config.json 的模型通道（复制后只校正路径）。
#   shared（演示用户真实数据）：data/workspace/skillsDir 指向正式安装目录
#     （注册表 AppId {BE7768E7-…} 的 InstallLocation）。
#     ★ 风险与安全口径：两个进程同写一个 jx.db（SQLite）会冲突，故启动前
#     检查「正式目录下的主程序进程」，在跑即拒绝启动（须先完全退出正式版，
#     含托盘）；config 为现场生成的副本（正式配置的相对 dataDir 在开发版
#     进程里会锚到 dev-app，必须绝对化），绝不回写正式配置。互斥体用
#     JX-Desktop-DevApp-Shared：单写者靠上面的启动拦截保证，不用默认互斥体
#     是因为它会把所有桌面实例（包括与正式数据无关的）都挡在外面。
# 计划任务友好：路径全绝对、无交互提示、Start-Transcript 落 dev-app\logs\，
#   失败 throw（退出码 1），schtasks 的「上次结果」可见。示例见
#   打包\开发桌面版-录屏演示-说明.md。
# 用法：powershell -NoProfile -ExecutionPolicy Bypass -File 打包\开发桌面版-录屏演示.ps1 [-DataMode isolated|shared] [-NoBuild]
#   或双击根目录「编译并启动AI数字助理-开发版.bat」（隔离）/「…-共享数据.bat」（共享）
param(
  [ValidateSet('isolated','shared')][string]$DataMode = 'isolated',
  [switch]$NoBuild
)

$ErrorActionPreference = "Stop"
$root    = Split-Path -Parent $PSScriptRoot
$devApp  = 'D:\智能体工作区\数字助理GO\dev-app'
$ExeName = 'AI数字助理.exe'   # ★ 撞名切换点：正式版改名后此处改 'AI数字助理-开发.exe'
$exe     = Join-Path $devApp $ExeName

# JSON 帮助：改写 config 文本而不做 ConvertFrom-Json 往返（往返会重排/改写
# 用户手动调过的配置）；Windows 路径按 JSON 规则转义反斜杠与引号
function To-JsonString([string]$s) { '"' + ($s -replace '\\', '\\' -replace '"', '\"') + '"' }
function Set-CfgValue([string]$text, [string]$key, [string]$value) {
  $lit = To-JsonString $value
  $pat = '"' + [regex]::Escape($key) + '"\s*:\s*"(?:[^"\\]|\\.)*"'
  if ([regex]::IsMatch($text, $pat)) {
    return [regex]::Replace($text, $pat, { param($m) '"' + $key + '": ' + $lit })
  }
  $brace = $text.IndexOf('{')
  if ($brace -lt 0) { return $text }
  return $text.Substring(0, $brace + 1) + "`r`n  `"$key`": $lit," + $text.Substring($brace + 1)
}
function Get-CfgValue([string]$text, [string]$key, [string]$fallback) {
  $m = [regex]::Match($text, '"' + [regex]::Escape($key) + '"\s*:\s*"((?:[^"\\]|\\.)*)"')
  if (-not $m.Success) { return $fallback }
  return ($m.Groups[1].Value -replace '\\\\', '\' -replace '\\"', '"')
}
# 关掉 remoteAccess.enabled（限定在 remoteAccess 对象内；[^{}] 不跨嵌套层级）。
# 隔离配置必用：config 从仓库根复制而来，remoteAccess.myId 是宿主的设备号，
# 同号注册会被网关互踢（relayguard.go 描述的问题），宿主的手机入口跟着闪断。
function Set-RemoteAccessOff([string]$text) {
  $pat = '("remoteAccess"\s*:\s*\{[^{}]*?"enabled"\s*:\s*)true'
  if ([regex]::IsMatch($text, $pat)) {
    return [regex]::Replace($text, $pat, '$1false', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
  }
  # 对象里没有 enabled 键（罕见）→ 插在对象开头
  if ([regex]::IsMatch($text, '"remoteAccess"\s*:\s*\{')) {
    return [regex]::Replace($text, '("remoteAccess"\s*:\s*\{)', '$1 "enabled": false,', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
  }
  return $text
}

# 日志先行（计划任务无控制台，输出必须落文件）
New-Item -ItemType Directory -Force -Path $devApp, (Join-Path $devApp 'logs') | Out-Null
$logPath = Join-Path $devApp ('logs\启动-{0}-{1}.log' -f $DataMode, (Get-Date -Format 'yyyyMMdd-HHmmss'))
Start-Transcript -Path $logPath | Out-Null
try {
Write-Host ("=== 开发桌面版·录屏演示  DataMode={0}  {1} ===" -f $DataMode, (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'))

# 1) 只关本脚本产物的旧实例（按完整路径匹配：正式版、其他桌面 exe、
#    仓库根的宿主进程一概不动）
$old = @(Get-CimInstance Win32_Process -Filter "Name='$ExeName'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -eq $exe })
foreach ($p in $old) {
  Write-Host ("关闭旧开发实例 pid=" + $p.ProcessId)
  try { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop } catch { }
}
if ($old.Count -gt 0) { Start-Sleep -Seconds 2 }

# 2) 组件目录联接（见头部注释：桌面版按 exe 旁相对路径发现组件/内置组件）
foreach ($name in @('组件', '内置组件')) {
  $link = Join-Path $devApp $name; $target = Join-Path $root $name
  if (-not (Test-Path $link) -and (Test-Path $target)) {
    New-Item -ItemType Junction -Path $link -Target $target | Out-Null
    Write-Host "已建目录联接：$link -> $target"
  }
}

# 3) go 工具链：优先本机绝对路径（计划任务会话的 PATH 可能没有 go），
#    并注入 PATH 让 编译语音输入法.ps1 内部的裸 go 也能解析
$goExe = @('D:\智能体工作区\toolchain\go\bin\go.exe') +
  @((Get-Command go -ErrorAction SilentlyContinue | ForEach-Object { $_.Source })) |
  Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $goExe) { throw '找不到 go.exe（D:\智能体工作区\toolchain\go\bin 与 PATH 均无）' }
$goDir = Split-Path -Parent $goExe
if (-not (@($env:PATH -split ';') -contains $goDir)) { $env:PATH = "$goDir;$env:PATH" }
Write-Host "go：$goExe"

# 4) 编译（-NoBuild：产物已就绪只重启）。GUI 子系统（录屏画面无控制台窗，
#    程序日志落 cwd\data\desktop.log 即 dev-app\data\desktop.log）+ 注入
#    desktopDefault=1（双击即桌面形态）+ appVersion 未来号（不弹自动更新）
if (-not $NoBuild) {
  & (Join-Path $PSScriptRoot '编译语音输入法.ps1')
  if ($LASTEXITCODE -ne 0) { throw '语音输入法编译失败' }
  # GUI 子系统（录屏画面无控制台窗，程序日志落 cwd\data\desktop.log 即
  # dev-app\data\desktop.log）+ 注入 desktopDefault=1（双击即桌面形态）+
  # appVersion 未来号（不弹自动更新）。注意必须 Push-Location 到仓库根：
  # go build 的模块定位从 cwd 开始（计划任务的 cwd 是 System32，锚定失败
  # 会报 go.mod file not found）
  Push-Location $root
  try {
    & $goExe build -trimpath -ldflags '-s -w -H=windowsgui -X main.desktopDefault=1 -X main.appVersion=20991231-1' -o $exe $root
    if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }
  } finally { Pop-Location }
  Write-Host ("编译完成：{0}（{1:N1} MB）" -f $exe, ((Get-Item $exe).Length / 1MB))
} elseif (-not (Test-Path $exe)) {
  throw "产物不存在（-NoBuild 只重启不编译）：$exe"
}

# 5) 按模式准备配置与互斥体
$utf8NoBom = New-Object System.Text.UTF8Encoding $false  # Go 的 json.Unmarshal 不吃 BOM
if ($DataMode -eq 'isolated') {
  $cfgPath = Join-Path $devApp 'config-隔离.json'
  $isoData = Join-Path $devApp 'data'; $isoWs = Join-Path $devApp '工作区'
  New-Item -ItemType Directory -Force -Path $isoData, $isoWs | Out-Null
  if (-not (Test-Path $cfgPath)) {
    $srcCfg = Join-Path $root 'config.json'
    if (Test-Path $srcCfg) {
      [IO.File]::WriteAllText($cfgPath, [IO.File]::ReadAllText($srcCfg), $utf8NoBom)
      Write-Host '首次：已沿用仓库根 config.json（模型通道原样保留），路径字段即将校正到 dev-app'
    } else {
      [IO.File]::WriteAllText($cfgPath, "{`r`n}`r`n", $utf8NoBom)
      Write-Host '首次：生成空配置（仓库根无 config.json），通道在界面里自行配置'
    }
  }
  # 每次启动都校正一遍：手改过/复制来的配置必须落在 dev-app 下，绝不写正式数据；
  # 远程中继必须关（同设备号会踢掉宿主/正式版的手机入口，见 Set-RemoteAccessOff）
  $text = [IO.File]::ReadAllText($cfgPath)
  $text = Set-RemoteAccessOff (Set-CfgValue (Set-CfgValue (Set-CfgValue $text 'dataDir' $isoData) 'workspace' $isoWs) 'skillsDir' (Join-Path $root '技能'))
  [IO.File]::WriteAllText($cfgPath, $text, $utf8NoBom)
  $env:JX_DESKTOP_MUTEX = 'JX-Desktop-DevApp-Isolated'
  Write-Host "模式【隔离数据】：$isoData / 工作区 $isoWs（互斥体 JX-Desktop-DevApp-Isolated，可与正式版同时开）"
} else {
  # —— 共享正式数据（演示口径与安全护栏见头部注释）——
  $prodDir = $null
  $reg = Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\{BE7768E7-93C5-49D6-9778-BC133A39A797}_is1' -ErrorAction SilentlyContinue
  if ($reg -and $reg.InstallLocation) { $prodDir = $reg.InstallLocation.TrimEnd('\') }
  if (-not $prodDir) {
    foreach ($cand in @("$env:LOCALAPPDATA\Programs\坚祥数字助理GO", "$env:LOCALAPPDATA\Programs\AI数字助理")) {
      if (Test-Path (Join-Path $cand 'config.json')) { $prodDir = $cand; break }
    }
  }
  if (-not $prodDir) { throw '未找到正式安装目录（注册表 InstallLocation 与 LOCALAPPDATA 候选均无），共享模式不可用' }
  $prodCfgPath = Join-Path $prodDir 'config.json'
  if (-not (Test-Path $prodCfgPath)) { throw "正式安装目录缺 config.json：$prodCfgPath" }
  # SQLite 单写者护栏：正式目录下的主程序进程在跑 → 拒绝启动
  $hold = @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object {
    ($_.Name -in @('坚祥数字助理GO.exe', 'AI数字助理.exe', 'jx-assistant.exe')) -and
    $_.ExecutablePath -and $_.ExecutablePath.StartsWith($prodDir + '\', [StringComparison]::OrdinalIgnoreCase) })
  if ($hold.Count -gt 0) {
    $why = ("共享数据模式拒绝启动：正式版实例正在运行（" +
      (($hold | ForEach-Object { "pid=$($_.ProcessId) $($_.ExecutablePath)" }) -join '；') +
      "）。同一 jx.db 两个进程写会冲突，请先完全退出正式版（含托盘）再启动本模式。")
    Write-Host $why
    throw $why
  }
  # 现场生成配置副本（每次重新生成，跟住正式配置的变化；绝不回写正式文件）
  $pText = [IO.File]::ReadAllText($prodCfgPath)
  $dd = Get-CfgValue $pText 'dataDir' 'data'
  if ($dd -notmatch '^[A-Za-z]:[\\/]') { $dd = [IO.Path]::GetFullPath((Join-Path $prodDir $dd)) }
  $ws = Get-CfgValue $pText 'workspace' ''
  if ($ws -eq '' -or $ws -notmatch '^[A-Za-z]:[\\/]') { $ws = Join-Path $prodDir '默认工作区' }
  $cfgPath = Join-Path $devApp 'config-共享.json'
  $text = Set-CfgValue (Set-CfgValue (Set-CfgValue $pText 'dataDir' $dd) 'workspace' $ws) 'skillsDir' (Join-Path $prodDir '技能')
  [IO.File]::WriteAllText($cfgPath, $text, $utf8NoBom)
  $env:JX_DESKTOP_MUTEX = 'JX-Desktop-DevApp-Shared'
  Write-Host "模式【共享正式数据】：$dd（正式目录 $prodDir；互斥体 JX-Desktop-DevApp-Shared）"
  Write-Host '口径：仅演示用——启动前已拦截正式实例保证单写者；请勿绕过脚本强开。'
}

# 6) 启动：随机高位端口（碰撞时程序自动回退系统分配）；-config 绝对路径
#    （桌面版 cwd 锚 exe 旁，相对路径必错）；GUI 子系统下程序日志在
#    dev-app\data\desktop.log
$env:JX_SKIP_MIGRATIONS = '1'   # 开发实例兜底闸：启动时整体跳过迁移（不读不写账本、不碰技能目录；正式版不设此变量，行为不变）
$port = Get-Random -Minimum 20000 -Maximum 60000
# 本次实际端口落固定文件：计划任务/外部脚本无从拿控制台输出，靠它接后续验证
Set-Content -Path (Join-Path $devApp 'logs\last-port.txt') -Value $port -Encoding ASCII
Start-Process -FilePath $exe -ArgumentList @('-desktop', '-config', "`"$cfgPath`"", '-port', "$port", '-noopen') -WorkingDirectory $devApp
Write-Host "已启动 $ExeName（预期端口 $port；程序日志 $devApp\data\desktop.log）"
Start-Sleep -Seconds 6
$proc = Get-CimInstance Win32_Process -Filter "Name='$ExeName'" -ErrorAction SilentlyContinue |
  Where-Object { $_.ExecutablePath -eq $exe }
if (-not $proc) { throw "启动后 6 秒进程不在：查 $devApp\data\desktop.log（互斥体冲突 / WebView2 / 端口）" }
$gp = Get-Process -Id $proc.ProcessId -ErrorAction SilentlyContinue
$title = if ($gp) { $gp.MainWindowTitle } else { '' }
Write-Host ("自检通过：pid=" + $proc.ProcessId + " 窗口标题=[" + $title + "]")
Write-Host "本次日志：$logPath"
exit 0
} finally {
  Stop-Transcript | Out-Null
}
