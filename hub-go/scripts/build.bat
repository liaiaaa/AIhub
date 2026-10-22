@echo off
rem ============================================================
rem  AIHub (Go) - build script
rem
rem  Usage: double-click, or run scripts\build.bat from anywhere.
rem  Output: <project root>\bin\aihub.exe
rem
rem  Notes:
rem   - Go toolchain path is configured below (edit GO if yours differs).
rem   - Build cache also lives on E: to avoid filling C:.
rem   - %~dp0 is this script's dir (scripts\); root is its parent.
rem   - IMPORTANT: keep .bat files ASCII-only + CRLF. cmd.exe on a
rem     Chinese Windows reads .bat as GBK; UTF-8 Chinese comments
rem     corrupt parsing and break the script (this actually happened).
rem ============================================================
setlocal

rem project root = parent of scripts\
set "ROOT=%~dp0.."
cd /d "%ROOT%" || (
  echo [ERROR] cannot enter project root: %ROOT%
  pause
  exit /b 1
)
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "GO=C:\Go\bin\go.exe"
if not exist "%GO%" (
  echo [ERROR] Go toolchain not found: %GO%
  echo Please install Go for Windows amd64 (default C:\Go) or edit GO below
  pause
  exit /b 1
)

set "GOCACHE=%LOCALAPPDATA%\aihub\go-cache"
set "GOPATH=%LOCALAPPDATA%\aihub\go-path"
if not exist "%GOCACHE%" mkdir "%GOCACHE%"
if not exist "%GOPATH%" mkdir "%GOPATH%"
if not exist "%ROOT%\bin" mkdir "%ROOT%\bin"

echo [1/2] Building... first run may take 1-2 minutes.
"%GO%" build -trimpath -ldflags "-s -w -H=windowsgui" -o "%ROOT%\bin\aihub.exe" .
if errorlevel 1 (
  echo.
  echo [ERROR] Build FAILED. See errors above.
  pause
  exit /b 1
)

echo [2/2] Build OK: %ROOT%\bin\aihub.exe
echo.
echo To start the console, run the launcher script in this folder.
pause