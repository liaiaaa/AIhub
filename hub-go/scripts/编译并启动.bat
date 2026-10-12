@echo off
rem ============================================================
rem  AIHub (Go) - build AND start  (desktop shortcut target)
rem
rem  1) rebuild the binary so you always run the latest code
rem  2) start it (console opens at http://127.0.0.1:8801)
rem  3) this window closes itself
rem
rem  If you only want to start without rebuilding, use
rem  bin\aihub.exe or the start-only launcher instead.
rem
rem  ASCII-only + CRLF on purpose (see build.bat notes).
rem ============================================================
setlocal
set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "GO=E:\software\go\bin\go.exe"
if not exist "%GO%" (
  echo [ERROR] Go toolchain not found: %GO%
  pause
  exit /b 1
)

set "GOCACHE=E:\software\go-cache"
set "GOPATH=E:\software\go-path"
if not exist "%GOCACHE%" mkdir "%GOCACHE%"
if not exist "%GOPATH%" mkdir "%GOPATH%"
if not exist "%ROOT%\bin" mkdir "%ROOT%\bin"

cd /d "%ROOT%"
echo Building (first run may take 1-2 minutes)...
"%GO%" build -trimpath -ldflags "-s -w -H=windowsgui" -o "%ROOT%\bin\aihub.exe" .
if errorlevel 1 (
  echo.
  echo [ERROR] Build FAILED. Not starting. See errors above.
  pause
  exit /b 1
)

echo Build OK. Starting...
start "" "%ROOT%\bin\aihub.exe"
exit /b 0