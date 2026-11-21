<#
  restart-assistant.ps1 —— 数字助理「自重启」脚本

  做什么：把正在运行的桌面版（默认 AI数字助理.exe，改名前的旧名坚祥数字助理GO.exe
  仍会探测）关掉，等几秒，再把它重新拉起来。

  为什么不能简单地「杀掉自己再启动」：
    引擎启动时会创建并挂入一个 kill-on-close 的 Job Object
    （见 processguard_windows.go），且未设 breakaway——主进程一终止，
    它派生过的所有进程（包括你刚启动的新实例）会被内核立即一起清掉。
    所以本脚本必须由 Job 之外的进程拉起：推荐入口是同目录的
    「重启数字助理.cmd」，双击即可（由 explorer 启动，天然在 Job 外）。

  用法：
    双击「重启数字助理.cmd」                                  默认 5 秒后动手
    restart-assistant.ps1 -DelaySec 20                       延迟 20 秒再关
    restart-assistant.ps1 -KillStray                         连无窗口的 -port/-noopen 实例一起关
    restart-assistant.ps1 -DryRun                            只演练，不真关不真起
    restart-assistant.ps1 -ExePath "D:\...\AI数字助理.exe"    指定别的可执行文件

  日志：<同目录>\.jxai\tmp\restart-assistant.log
#>
[CmdletBinding()]
param(
    [int]$DelaySec        = 5,      # 脚本启动后先等多久再动手（给在途请求/回复留时间）
    [int]$KillWaitSec     = 3,      # 关掉旧实例后等多久再拉起新的
    [int]$ReadyTimeoutSec = 30,     # 等待新实例窗口就绪的上限
    [string]$ExePath      = '',     # 默认按脚本同目录探测：AI数字助理.exe → 坚祥数字助理GO.exe（旧名回退）
    [switch]$KillStray,             # 同时关闭无窗口的调试实例（命令行带 -port / -noopen）
    [switch]$DryRun                 # 演练模式：只打印计划，不关也不起
)

$ErrorActionPreference = 'Continue'

# ---------- 路径与工具 ----------
$root = $PSScriptRoot
if (-not $root) { $root = Split-Path -Parent $MyInvocation.MyCommand.Path }
if (-not $ExePath) {
    # exe 已切新名（AI数字助理，2026-11-19）：新名优先，旧名回退（迁移前的存量机器）
    $try = Join-Path $root 'AI数字助理.exe'
    if (-not (Test-Path -LiteralPath $try)) { $try = Join-Path $root '坚祥数字助理GO.exe' }
    $ExePath = $try
}
$logDir   = Join-Path $root '.jxai\tmp'
$logFile  = Join-Path $logDir 'restart-assistant.log'
$noBomUtf8 = New-Object System.Text.UTF8Encoding($false)

function Write-Log([string]$msg) {
    $line = ('[{0}] {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $msg) + "`r`n"
    try {
        if (-not (Test-Path -LiteralPath $logDir)) { New-Item -ItemType Directory -Path $logDir -Force | Out-Null }
        [System.IO.File]::AppendAllText($logFile, $line, $noBomUtf8)
    } catch { }
}

function Get-AncestorChain([int]$startPid) {
    $chain = @()
    $cur = $startPid
    for ($i = 0; $i -lt 8 -and $cur -gt 0; $i++) {
        $p = Get-CimInstance Win32_Process -Filter "ProcessId=$cur" -ErrorAction SilentlyContinue
        if (-not $p) { break }
        $chain += ('{0}:{1}' -f $p.ProcessId, $p.Name)
        $cur = [int]$p.ParentProcessId
    }
    return ($chain -join ' <- ')
}

# 按 $ExePath 的文件名找进程：换成别的产物名（或别的安装目录）也不用改这里
function Get-JxProcesses {
    $procName = [System.IO.Path]::GetFileName($ExePath)
    return @(Get-CimInstance Win32_Process -Filter "Name='$procName'" -ErrorAction SilentlyContinue)
}

function Test-IsDesktopInstance($p) {
    $cl = [string]$p.CommandLine
    return ($cl -notmatch '(?i)-port' -and $cl -notmatch '(?i)-noopen')
}

# ---------- 开场记录 ----------
Write-Log '==================== 自重启任务开始 ===================='
Write-Log ('脚本 pid={0}；调用链 {1}' -f $PID, (Get-AncestorChain $PID))
Write-Log ('参数：DelaySec={0} KillWaitSec={1} KillStray={2} DryRun={3}' -f $DelaySec, $KillWaitSec, $KillStray.IsPresent, $DryRun.IsPresent)
Write-Log ('目标可执行文件：{0}（存在={1}）' -f $ExePath, (Test-Path -LiteralPath $ExePath))
if (-not (Test-Path -LiteralPath $ExePath)) { Write-Log ('中止：找不到 ' + $ExePath); exit 1 }

if ($DelaySec -gt 0) {
    Write-Log ('等待 {0} 秒后开始关闭旧实例…' -f $DelaySec)
    Start-Sleep -Seconds $DelaySec
}

# ---------- 找出要关掉的实例 ----------
$all     = Get-JxProcesses
$desktop = @($all | Where-Object { Test-IsDesktopInstance $_ })
$stray   = @($all | Where-Object { -not (Test-IsDesktopInstance $_) })
Write-Log ('当前实例：桌面实例 {0} 个；无窗口调试实例 {1} 个' -f $desktop.Count, $stray.Count)
if ($stray.Count -gt 0 -and -not $KillStray) {
    Write-Log ('（保留，未处理：pid {0}；需要一起关请加 -KillStray）' -f (($stray | ForEach-Object { $_.ProcessId }) -join ', '))
}

$targets = @()
if ($KillStray) { $targets = @($all) } else { $targets = @($desktop) }
$oldPids = @($targets | ForEach-Object { [int]$_.ProcessId })

foreach ($p in $targets) {
    if ($DryRun) { Write-Log ('[演练] 将关闭 pid={0}' -f $p.ProcessId); continue }
    Write-Log ('关闭 pid={0} …' -f $p.ProcessId)
    try { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop }
    catch { Write-Log ('  关闭失败：{0}' -f $_.Exception.Message) }
}

# ---------- 确认旧实例真的退出 ----------
if (-not $DryRun -and $oldPids.Count -gt 0) {
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        $alive = @(Get-JxProcesses | Where-Object { $oldPids -contains [int]$_.ProcessId })
        if ($alive.Count -eq 0) { break }
        Start-Sleep -Milliseconds 500
    }
    $alive = @(Get-JxProcesses | Where-Object { $oldPids -contains [int]$_.ProcessId })
    if ($alive.Count -gt 0) { Write-Log ('警告：仍有 {0} 个旧实例未退出，继续启动新实例' -f $alive.Count) }
    else { Write-Log '旧实例已全部退出' }
}

if ($DryRun) { Write-Log '[演练] 到此为止，不启动新实例'; exit 0 }

if ($KillWaitSec -gt 0) {
    Write-Log ('等待 {0} 秒后启动新实例…' -f $KillWaitSec)
    Start-Sleep -Seconds $KillWaitSec
}

# ---------- 拉起新实例（失败自动重试一次） ----------
$launchedPid = 0
for ($attempt = 1; $attempt -le 2; $attempt++) {
    Write-Log ('启动 {0}（第 {1} 次）' -f $ExePath, $attempt)
    try {
        $proc = Start-Process -FilePath $ExePath -WorkingDirectory $root -PassThru -ErrorAction Stop
        $launchedPid = $proc.Id
    } catch {
        Write-Log ('启动失败：{0}' -f $_.Exception.Message)
        $launchedPid = 0
        break
    }
    Start-Sleep -Seconds 6
    if (Get-Process -Id $launchedPid -ErrorAction SilentlyContinue) { break }
    Write-Log ('pid={0} 启动后立即退出（可能被已有实例接管），重试…' -f $launchedPid)
    $launchedPid = 0
}

if ($launchedPid -eq 0) { Write-Log '重启失败：新实例没能起来'; exit 2 }

# ---------- 等窗口就绪 ----------
$deadline = (Get-Date).AddSeconds($ReadyTimeoutSec)
$winReady = $false
while ((Get-Date) -lt $deadline) {
    $proc = Get-Process -Id $launchedPid -ErrorAction SilentlyContinue
    if (-not $proc) { break }
    if ($proc.MainWindowHandle -ne 0) { $winReady = $true; break }
    Start-Sleep -Milliseconds 500
}
Write-Log ('新实例 pid={0}，主窗口就绪={1}' -f $launchedPid, $winReady)
Write-Log '重启完成。'
"restarted pid=$launchedPid window=$winReady"
