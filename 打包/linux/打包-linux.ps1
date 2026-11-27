# Linux amd64 发行包打包脚本（发版用，仓库根目录运行）
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File 打包\linux\打包-linux.ps1
#   powershell ... -NoRuntime     # 只出纯二进制包（无技能区，约 19MB）
#
# 产物：打包\linux\jx-assistant-linux-amd64.tar.gz
#   布局：jx-assistant-linux-amd64 / README-Linux.md / jx-assistant.service 平铺
#         + 技能/（内置技能 + _runtime/linux 便携 Python 运行时，约 670MB 解压）
#   精简口径（2026-11-27 用户定）：Go 主程序 + Python 运行时——不带语音组件
#   （xasr 模型 / sherpa-onnx）、不带「操作电脑」内置组件（cua / windows-mcp）、
#   不带用户技能与用户组件，与 Windows 全量安装包的差异见 README-Linux.md。
#
# 依赖：本机 WSL（tar 必须在 Linux 侧打——Windows bsdtar 从 NTFS 打包会丢
#   执行位与 symlink，装出去的 python3 打不开）。WSL 侧组装/校验/打包全部
#   在 pkg-prepare.sh 里完成（本脚本负责编译与 stage 暂存）。
#   wsl.exe 自身会把 .wslconfig 之类的警告打到 stderr，一律 2>$null 丢弃，
#   失败判定只认退出码与 stdout 标记。
param(
  [string]$OutDir = "$PSScriptRoot",
  [switch]$NoRuntime,
  [string]$WSLDistro = "Ubuntu-24.04"
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent
Set-Location $repoRoot

function Win2Wsl([string]$p) {
  $p = $p -replace '\\', '/'
  $drive = $p.Substring(0, 1).ToLower()
  $p = $p -replace '^[A-Za-z]:', ('/mnt/' + $drive)
  return ($p -replace '/+', '/')
}

$stage = Join-Path $OutDir 'stage-linux'
if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item $stage -ItemType Directory | Out-Null

Write-Host "== 交叉编译 linux/amd64"
$env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
go build -trimpath -ldflags "-s -w" -o (Join-Path $stage 'jx-assistant-linux-amd64') .
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
$env:GOOS = ''; $env:GOARCH = ''; $env:CGO_ENABLED = ''

Copy-Item (Join-Path $OutDir 'README-Linux.md') $stage
Copy-Item (Join-Path $OutDir 'jx-assistant.service') $stage

if (-not $NoRuntime) {
  Write-Host "== 暂存技能区（内置技能 + _runtime/linux 便携运行时，体积较大）"
  $srcSkills = Join-Path $repoRoot '技能'
  $stageSkills = Join-Path $stage '技能'
  New-Item $stageSkills -ItemType Directory | Out-Null

  # 结构硬校验（Windows 侧能验的先验掉；import 级校验在 WSL 的 pkg-prepare.sh 里）
  $rtBin = Join-Path $srcSkills '_runtime\linux\bin\python3.14'
  if (-not (Test-Path $rtBin)) {
    throw "便携运行时缺失：$rtBin（装配：WSL 里 python-build-standalone install_only 解压 + pip 装内置技能依赖，拷入后跑 sanitize 把 symlink 转文本形态——见 同目录脚本与 AGENT.md）"
  }
  $spRoot = Join-Path $srcSkills '_runtime\linux\lib\python3.14\site-packages'
  # Windows 残留防呆（2026-11-27 实测踩坑：该目录历史曾被装进一份 Windows embeddable，
  # 与 Linux 布局在同一 NTFS 目录混居（lib/Lib 大小写合并）后打出的包全废——顶层出现
  # 任何 Windows 运行时特征一律拒绝出包，整目录改名隔离后从装配源重建）
  $rtTop = Join-Path $srcSkills '_runtime\linux'
  $bad = Get-ChildItem $rtTop -Force | Where-Object {
    $_.Name -ceq 'Lib' -or $_.Name -ceq 'Scripts' -or
    $_.Extension -in @('.exe', '.pyd', '.dll', '._pth', '.zip', '.cat') -or
    $_.Name -cmatch '^python'
  }
  if ($bad) {
    throw "运行时顶层有 Windows 残留（$($bad.Name -join '、')），拒绝出包：整目录改名隔离后从装配源重建（详见 打包\linux\README-Linux.md）"
  }
  foreach ($pkg in @('rapidocr_onnxruntime', 'docx', 'openpyxl', 'pptx', 'pypdfium2', 'pdfplumber', 'pypdf', 'numpy', 'cv2')) {
    if (-not (Test-Path (Join-Path $spRoot $pkg))) {
      throw "运行时缺内置技能依赖包：site-packages/$pkg（对齐各技能 manifest 声明，缺一不出包）"
    }
  }

  # 内置技能：仓库 技能\ 下非 user / 非 _runtime 的目录（与内嵌母本同一批）
  $builtin = Get-ChildItem $srcSkills -Directory | Where-Object {
    $_.Name -ne 'user' -and $_.Name -ne '_runtime'
  }
  if ($builtin.Count -eq 0) { throw "仓库 技能\ 下没有内置技能目录，拒绝出包" }
  foreach ($sk in $builtin) {
    robocopy $sk.FullName (Join-Path $stageSkills $sk.Name) /E /R:0 /W:0 /NFL /NDL /NJH /NJS
    if ($LASTEXITCODE -ge 8) { throw "复制技能目录失败：$($sk.Name)（退出码 $LASTEXITCODE）" }
    if (-not (Test-Path (Join-Path $stageSkills ($sk.Name + '\manifest.json')))) {
      throw "技能 $($sk.Name) 缺 manifest.json，拒绝出包"
    }
  }
  # 便携运行时（排除 __pycache__/*.pyc：跨机器无效且白占体积）
  robocopy (Join-Path $srcSkills '_runtime\linux') (Join-Path $stageSkills '_runtime\linux') `
    /E /R:0 /W:0 /NFL /NDL /NJH /NJS /XD __pycache__ /XF *.pyc
  if ($LASTEXITCODE -ge 8) { throw "复制 _runtime\linux 失败（退出码 $LASTEXITCODE）" }
}

# ---- WSL 侧组装 + 校验 + tar 打包（pkg-prepare.sh）----
$pkg = Join-Path $OutDir 'jx-assistant-linux-amd64.tar.gz'
if (Test-Path $pkg) { Remove-Item $pkg -Force }
Write-Host "== WSL 组装/校验/打包（pkg-prepare.sh，体积较大耐心等待）"
$withRt = if ($NoRuntime) { '0' } else { '1' }
$prepScript = Join-Path $OutDir 'pkg-prepare.sh'
if (-not (Test-Path $prepScript)) { throw "缺少 $prepScript" }
wsl.exe -d $WSLDistro -- sh (Win2Wsl $prepScript) (Win2Wsl $stage) (Win2Wsl $pkg) $withRt 2>$null
if ($LASTEXITCODE -ne 0) { throw "pkg-prepare.sh 失败（退出码 $LASTEXITCODE；重跑去掉 2>`$null 看细节）" }

Remove-Item $stage -Recurse -Force

if (-not (Test-Path $pkg)) { throw "产物未生成：$pkg" }
$size = (Get-Item $pkg).Length
Write-Host ("完成: {0}（{1:N1} MB）" -f $pkg, ($size / 1MB))
Write-Host "sha256: $((Get-FileHash $pkg -Algorithm SHA256).Hash.ToLower())"
