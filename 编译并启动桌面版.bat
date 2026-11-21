@echo off
rem ============================================================
rem  编译并启动桌面版（开发用，双击运行）
rem  1) 调用 打包\开发桌面版.ps1 构建仓库根的 AI数字助理.exe
rem  2) 构建成功后直接拉起桌面窗口
rem  注意：桌面壳是单实例——程序开着时重建 exe 会因文件被占用而
rem  失败，此时先关掉正在运行的桌面版窗口再双击本脚本。
rem  （本文件须保存为 ANSI/GBK 编码：cmd 按 GBK 解析批处理）
rem ============================================================
cd /d "%~dp0"
echo 正在编译桌面版...
powershell -NoProfile -ExecutionPolicy Bypass -File "打包\开发桌面版.ps1"
if errorlevel 1 (
  echo.
  echo [失败] 编译未通过。若提示无法写入 exe，请先关闭正在运行的桌面版窗口再试。
  echo.
  pause
  exit /b 1
)
echo 编译完成，正在启动...
start "" "%~dp0AI数字助理.exe"
exit /b 0
