@echo off
rem ============================================================
rem  编译并启动 AI数字助理.exe 开发版（共享正式数据，演示用）
rem  读取正式安装目录的用户真实数据（会话/充值状态）。
rem  ★ 两个进程同写一个 jx.db 会冲突：脚本会先检查正式版是否
rem    在运行，在跑即拒绝启动——请先完全退出正式版（含托盘）。
rem  （本文件须保持 ANSI/GBK 编码：cmd 按 GBK 解析批处理）
rem ============================================================
cd /d "%~dp0"
echo 正在编译并启动 AI数字助理.exe 开发版（共享正式数据）...
powershell -NoProfile -ExecutionPolicy Bypass -File "打包\开发桌面版-录屏演示.ps1" -DataMode shared
if errorlevel 1 (
  echo.
  echo [失败] 若提示正式版正在运行：请先完全退出正式版（含托盘）再双击本文件。
  echo        其他详情见 D:\智能体工作区\数字助理GO\dev-app\logs 下的启动日志。
  pause
  exit /b 1
)
exit /b 0