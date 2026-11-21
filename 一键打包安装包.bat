@echo off
rem ============================================================
rem  一键打包桌面版安装包（双击运行）
rem  流程 = 打包\打包安装包.ps1：编译主程序、组装 stage（技能/组件/
rem         MD查看器等）、Inno Setup 压缩出安装包
rem  产物 = 打包\输出\AI数字助理-安装包-<版本>.exe
rem         （版本默认=当天日期-1，同日重打包自动覆盖同名文件）
rem  前置：本机已装 Inno Setup 6 与 Go 工具链；打包前最好先退出正在
rem        运行的程序（AI数字助理/旧名坚祥数字助理GO，exe 被占用会导致编译失败）。
rem  本文件必须保存为 ANSI/GBK 编码：cmd 是 GBK 环境（同 编译并启动桌面版.bat）。
rem ============================================================
cd /d "%~dp0"
if not exist "打包\打包安装包.ps1" (
  echo [失败] 找不到 打包\打包安装包.ps1 —— 请把本脚本放回仓库根目录再运行。
  pause
  exit /b 1
)
echo.
echo == 开始打包桌面版安装包：编译主程序 - 组装文件 - Inno Setup 压缩
echo    全程约需几分钟到十几分钟（取决于机器性能），请勿关闭本窗口...
echo.
powershell -NoProfile -ExecutionPolicy Bypass -File "打包\打包安装包.ps1"
if errorlevel 1 goto :failed

echo.
echo == 打包完成！安装包在【打包\输出】目录（按版本号命名）。
start "" explorer "%~dp0打包\输出"
echo.
pause
exit /b 0

:failed
echo.
echo == [失败] 打包未完成，请看上方报错。常见原因：
echo    1) 没装 Inno Setup 6 —— 先装：winget install JRSoftware.InnoSetup
echo    2) AI数字助理（或旧名坚祥数字助理GO）正在运行占用了 exe —— 先退出程序再重试
echo    3) Go 工具链不在 PATH —— 命令行跑 go version 应能出版本号
echo.
pause
exit /b 1
