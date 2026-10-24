@echo off
cd /d "%~dp0codex-mobile"

if not exist "codex-mobile.exe" (
  echo [ERROR] codex-mobile.exe not found in %CD%
  pause
  exit /b 1
)

rem This is a tray app: no window stays open. Look for the icon in the
rem notification area (bottom right). Windows 11 may hide it under the "^".
start "" "codex-mobile.exe"
exit
