# Build MD Viewer (MDViewer.exe) as a SINGLE self-contained exe and sync it
# into the Jianxiang repo. Windows PowerShell 5.1 compatible; ASCII-only
# (Chinese file names are spelled via code points, mirroring
# tools\launcher\build-launcher.ps1).
#
# Steps:
#   1) borrow WebView2 assemblies from the launcher's lib (repo root fallback,
#      NuGet extraction last resort)
#   2) app-logo.png -> app.ico via make-icon.cs
#   3) inject the shared marked build (web/marked.lib.js, the repo's single
#      copy) into the viewer template's /*__MARKED__*/ slot, then
#   4) compile ONE exe: WebView2 managed assemblies, native loader, and the
#      viewer template all embedded as resources (AssemblyResolve loads the
#      managed ones at runtime; the loader drops to %LOCALAPPDATA%)
#   5) ship exe + manual into <repo>\md-viewer\ (nothing else)
$ErrorActionPreference = 'Stop'

$devDir   = $PSScriptRoot
$buildDir = Join-Path $devDir 'build'
$libDir   = Join-Path $devDir 'lib'
$srcDir   = Join-Path $devDir 'src'

# "坚祥数字助理" spelled via code points so this script stays pure ASCII.
$repoName = [string][char]0x575A + [char]0x7965 + [char]0x6570 + [char]0x5B57 + [char]0x52A9 + [char]0x7406
# "MD查看器" ditto.
$appName  = 'MD' + [string][char]0x67E5 + [char]0x770B + [char]0x5668
# "使用说明.txt" ditto.
$manualName = [string][char]0x4F7F + [char]0x7528 + [char]0x8BF4 + [char]0x660E + '.txt'

$repoRoot = Split-Path -Parent $devDir
$repoDir  = Join-Path $repoRoot $repoName
if (-not (Test-Path $repoDir)) { throw "repo root not found: $repoDir" }
$shipDir  = Join-Path $repoDir 'md-viewer'

$csc = 'C:\Windows\Microsoft.NET\Framework64\v4.0.30319\csc.exe'
if (-not (Test-Path $csc)) { throw "csc.exe not found at $csc (.NET Framework 4.x compiler required)." }

# ---------- Prerequisites: WebView2 assemblies ----------
New-Item -ItemType Directory -Force -Path $libDir, $buildDir | Out-Null
$coreDll     = Join-Path $libDir 'Microsoft.Web.WebView2.Core.dll'
$winFormsDll = Join-Path $libDir 'Microsoft.Web.WebView2.WinForms.dll'
$loaderDll   = Join-Path $libDir 'WebView2Loader.dll'
$templateIn  = Join-Path $devDir 'viewer-template.html'
$welcomeIn   = Join-Path $srcDir 'welcome.html'

$missing = @($coreDll, $winFormsDll, $loaderDll) | Where-Object { -not (Test-Path $_) }
if ($missing.Count -gt 0) {
    $launcherLib = Join-Path $repoDir 'tools\launcher\lib'
    $repoCopy    = $repoDir
    $nugetPkgs   = Join-Path $repoDir 'tools\launcher\packages\Microsoft.Web.WebView2'
    $copied = $false
    foreach ($src in @($launcherLib, $repoCopy)) {
        if ((Test-Path (Join-Path $src 'Microsoft.Web.WebView2.Core.dll'))) {
            Write-Host "[..] Borrowing WebView2 assemblies from $src"
            Copy-Item (Join-Path $src 'Microsoft.Web.WebView2.Core.dll')     $coreDll     -Force
            Copy-Item (Join-Path $src 'Microsoft.Web.WebView2.WinForms.dll') $winFormsDll -Force
            Copy-Item (Join-Path $src 'WebView2Loader.dll')                  $loaderDll   -Force
            $copied = $true
            break
        }
    }
    if (-not $copied) {
        if (-not (Test-Path $nugetPkgs)) {
            throw "WebView2 assemblies missing and no source found (launcher lib, repo root, NuGet package)."
        }
        Write-Host "[..] Extracting WebView2 assemblies from NuGet package ..."
        Copy-Item (Join-Path $nugetPkgs 'lib\net462\Microsoft.Web.WebView2.Core.dll')     $coreDll     -Force
        Copy-Item (Join-Path $nugetPkgs 'lib\net462\Microsoft.Web.WebView2.WinForms.dll') $winFormsDll -Force
        Copy-Item (Join-Path $nugetPkgs 'runtimes\win-x64\native\WebView2Loader.dll')     $loaderDll   -Force
    }
}
if (-not (Test-Path $templateIn)) { throw "viewer template missing: $templateIn" }
if (-not (Test-Path $welcomeIn)) { throw "welcome page missing: $welcomeIn" }

# ---------- 0) Inject marked into the template ----------
# marked exists exactly once in the repo (web/marked.lib.js). The template
# carries only the /*__MARKED__*/ slot; the exe must embed a self-contained
# page, so the build injects the same copy the main app injects at startup
# (mdview.go). Output goes to build\, never touching the source template.
$markedLibPath = Join-Path $repoDir 'web\marked.lib.js'
if (-not (Test-Path $markedLibPath)) { throw "marked lib missing: $markedLibPath" }
$markedJs   = [IO.File]::ReadAllText($markedLibPath)
$templateRaw = [IO.File]::ReadAllText($templateIn)
if ($templateRaw -notmatch '__MARKED__') { throw "viewer template missing the /*__MARKED__*/ slot: $templateIn" }
if ($markedJs -match '</script') { throw "marked lib contains '</script', cannot inline" }
$templateBuilt = Join-Path $buildDir 'viewer-template.html'
[IO.File]::WriteAllText($templateBuilt, $templateRaw.Replace('/*__MARKED__*/', $markedJs))
Write-Host "[0/3] Injected marked ($($markedJs.Length) chars) -> $templateBuilt"

# ---------- 1) Generate app.ico via make-icon ----------
Write-Host "[1/3] Generating app.ico ..."
$makeIconExe = Join-Path $buildDir 'make-icon.exe'
& $csc /nologo /target:exe /optimize+ `
    /out:$makeIconExe `
    /r:System.Drawing.dll `
    (Join-Path $srcDir 'make-icon.cs')
if ($LASTEXITCODE -ne 0) { throw "make-icon compilation failed (exit $LASTEXITCODE)" }

$iconSrc = Join-Path $devDir 'app-logo.png'
$iconOut = Join-Path $buildDir 'app.ico'
& $makeIconExe $iconSrc $iconOut
if ($LASTEXITCODE -ne 0) { throw "icon generation failed (exit $LASTEXITCODE)" }
if (-not (Test-Path $iconOut)) { throw "app.ico was not produced: $iconOut" }

# ---------- 2) Compile the single-exe viewer (GUI, x64, embedded resources) ----------
$exeName = $appName + '.exe'
$exeOut  = Join-Path $devDir $exeName
$manifestIn = Join-Path $srcDir 'app.manifest'
Write-Host "[2/3] Compiling $exeName (single file, resources embedded) ..."
& $csc /nologo /target:winexe /platform:x64 /optimize+ `
    /codepage:65001 `
    /win32icon:$iconOut `
    /win32manifest:$manifestIn `
    /out:$exeOut `
    /r:System.Windows.Forms.dll `
    /r:System.Drawing.dll `
    /r:System.Core.dll `
    /r:$coreDll `
    /r:$winFormsDll `
    "/resource:$coreDll,MDViewer.res.Microsoft.Web.WebView2.Core.dll" `
    "/resource:$winFormsDll,MDViewer.res.Microsoft.Web.WebView2.WinForms.dll" `
    "/resource:$loaderDll,MDViewer.res.WebView2Loader.dll" `
    "/resource:$templateBuilt,MDViewer.res.viewer-template.html" `
    "/resource:$welcomeIn,MDViewer.res.welcome.html" `
    (Join-Path $srcDir 'MDViewer.cs')
if ($LASTEXITCODE -ne 0) { throw "viewer compilation failed (exit $LASTEXITCODE)" }

# ---------- 3) Ship the single exe (+ manual) under the repo ----------
Write-Host "[3/3] Assembling $shipDir ..."
New-Item -ItemType Directory -Force -Path $shipDir | Out-Null
Get-ChildItem -LiteralPath $shipDir -Force | Remove-Item -Recurse -Force
Copy-Item -LiteralPath $exeOut -Destination $shipDir -Force
Copy-Item -LiteralPath (Join-Path $devDir $manualName) -Destination $shipDir -Force

$exeInfo = Get-Item $exeOut
Write-Host ""
Write-Host "OK: build finished -> $exeOut"
Write-Host "    size $([math]::Round($exeInfo.Length / 1KB, 0)) KB (single exe, everything embedded)"
Write-Host "    shippable folder -> $shipDir"
Get-ChildItem -LiteralPath $shipDir | ForEach-Object { Write-Host ("    - " + $_.Name) }
