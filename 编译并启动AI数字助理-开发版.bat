@echo off
rem ============================================================
rem  编译并启动 AI数字助理.exe 开发版（隔离数据，录屏演示用）
rem  产物在 D:\智能体工作区\数字助理GO\dev-app\AI数字助理.exe，
rem  与正式安装目录分开；exe 名与安装包 taskkill 名单不同，
rem  录屏中途跑安装包不会被关掉。数据落在 dev-app\data（隔离），
rem  可与正式版同时开。
rem  （本文件须保持 ANSI/GBK 编码：cmd 按 GBK 解析批处理）
rem ============================================================
cd /d "%~dp0"
echo 正在编译并启动 AI数字助理.exe 开发版（隔离数据）...
powershell -NoProfile -ExecutionPolicy Bypass -File "打包\开发桌面版-录屏演示.ps1" -DataMode isolated
if errorlevel 1 (
  echo.
  echo [失败] 详情见 D:\智能体工作区\数字助理GO\dev-app\logs 下的启动日志。
  pause
  exit /b 1
)
exit /b 0