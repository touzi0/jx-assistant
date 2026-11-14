@echo off
rem ============================================================
rem  Start Electron desktop shell (double-click to run)
rem  Engine = repo-root jx-assistant.exe (latest build).
rem  If you changed Go/frontend code, rebuild first with
rem  the dev script in the build-scripts folder (da-bao) (see AGENT.md), then
rem  use this bat for fast daily launch.
rem  NOTE: keep this file pure ASCII + CRLF. cmd parses bat
rem  in ANSI/GBK; UTF-8 Chinese or bare-LF line endings
rem  corrupt commands (a corrupted fragment once launched the
rem  engine in browser mode = backend + webpage, not the app).
rem ============================================================
set "JX_GO_EXE=%~dp0jx-assistant.exe"
if not exist "%JX_GO_EXE%" (
  echo [ERROR] jx-assistant.exe not found in repo root.
  echo The pre-fix binary was archived to the project archive folder ^(see AGENT.md^) -
  echo do NOT restore and run it: stale migration logic re-runs migrations
  echo against the skills dir and deletes skills. Rebuild a fresh engine first:
  echo run the Electron build script in the build-scripts ^(da-bao^)/ folder.
  pause
  exit /b 1
)
rem Stale-binary guard: the engine must contain the ledger-scoping fix
rem (binary string ".migrations.json"). Otherwise it is the old migration
rem logic and must not be launched.
powershell -NoProfile -Command "$t=[Text.Encoding]::ASCII.GetString([IO.File]::ReadAllBytes($env:JX_GO_EXE)); if(-not $t.Contains('.migrations.json')){ exit 1 }"
if errorlevel 1 (
  echo [ERROR] engine binary is stale ^(pre ledger-scoping fix^): it would
  echo re-run migrations against the skills dir and delete skills.
  echo Archive it ^(see AGENT.md^) and rebuild with the Electron build script.
  pause
  exit /b 1
)
cd /d "%~dp0..\jx-desktop"
if not exist "node_modules\electron\dist\electron.exe" (
  echo [ERROR] Electron not installed in jx-desktop. Run the dev script first.
  pause
  exit /b 1
)
npx electron .
