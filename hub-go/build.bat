@echo off
rem Build Go version of WorkBuddy Hub.
rem Go toolchain is at E:\software\go (no install to C:, no PATH change).
cd /d "%~dp0"
set GO=E:\software\go\bin\go.exe
if not exist "%GO%" (
  echo [ERROR] Go toolchain not found: %GO%
  echo Please download Go for Windows amd64 and extract to E:\software\go
  pause
  exit /b 1
)
rem Keep build caches on E: drive (avoid writing to C:).
set GOCACHE=E:\software\go-cache
set GOPATH=E:\software\go-path
if not exist "%GOCACHE%" mkdir "%GOCACHE%"
if not exist "%GOPATH%" mkdir "%GOPATH%"
echo Building... first run may take 1-2 minutes.
"%GO%" build -trimpath -ldflags "-s -w -H=windowsgui" -o workbuddy-hub.exe .
if %errorlevel%==0 (
  echo Build OK: %~dp0workbuddy-hub.exe
) else (
  echo Build FAILED. See errors above.
)
pause
