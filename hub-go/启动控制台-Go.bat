@echo off
rem Launch WorkBuddy Hub (Go). Default: all proxies stopped.
rem The browser opens the console page, then you choose which proxy to start.
cd /d "%~dp0"
if not exist "workbuddy-hub.exe" (
  echo [ERROR] workbuddy-hub.exe not found. Run build.bat first.
  pause
  exit /b 1
)
start "" "workbuddy-hub.exe"
