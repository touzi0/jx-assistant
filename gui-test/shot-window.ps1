# 截取「AI数字助理」主窗口（按 PID 主窗口句柄定位，避免中文标题编码问题）。
# 用法: powershell -File shot-window.ps1 <输出png> [pid] [激活窗口:1/0]
param(
  [Parameter(Mandatory = $true)][string]$OutFile,
  [int]$ProcId = 17244,
  [int]$Activate = 1
)
Add-Type -AssemblyName System.Drawing
Add-Type @"
using System;
using System.Runtime.InteropServices;
public class Win32 {
  [DllImport("user32.dll")] public static extern IntPtr FindWindowW(string cls, string title);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int cmd);
  [DllImport("user32.dll")] public static extern bool IsIconic(IntPtr h);
  public struct RECT { public int Left, Top, Right, Bottom; }
}
"@
$h = (Get-Process -Id $ProcId -ErrorAction SilentlyContinue).MainWindowHandle
if (-not $h -or $h -eq [IntPtr]::Zero) { Write-Error "window not found for pid $ProcId"; exit 1 }
if ($Activate -eq 1) {
  if ([Win32]::IsIconic($h)) { [void][Win32]::ShowWindow($h, 9) } # SW_RESTORE
  [void][Win32]::SetForegroundWindow($h)
  Start-Sleep -Milliseconds 600
}
$r = New-Object Win32+RECT
[void][Win32]::GetWindowRect($h, [ref]$r)
$w = $r.Right - $r.Left; $ht = $r.Bottom - $r.Top
if ($w -le 0 -or $ht -le 0) { Write-Error "bad rect ${w}x${ht}"; exit 1 }
$bmp = New-Object System.Drawing.Bitmap($w, $ht)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($r.Left, $r.Top, 0, 0, (New-Object System.Drawing.Size($w, $ht)))
$g.Dispose()
$bmp.Save($OutFile, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
Write-Output "saved $OutFile ${w}x${ht} at ($($r.Left),$($r.Top))"
