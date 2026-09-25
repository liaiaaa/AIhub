@echo off
rem ============================================================
rem  WorkBuddy Hub (Go) - 构建脚本
rem
rem  用法：双击本文件，或从任意目录执行 scripts\build.bat
rem  产物：<项目根>\bin\workbuddy-hub.exe
rem
rem  说明：
rem   - Go 工具链固定在 E:\software\go（不装 C 盘、不改 PATH）
rem   - 构建缓存也放 E 盘，避免占用 C 盘空间
rem   - %~dp0 是本脚本所在目录（scripts\），项目根是它的上一级
rem ============================================================
setlocal

rem 项目根 = scripts\ 的上一级
set "ROOT=%~dp0.."
cd /d "%ROOT%" || (
  echo [ERROR] 无法进入项目根目录: %ROOT%
  pause
  exit /b 1
)
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "GO=E:\software\go\bin\go.exe"
if not exist "%GO%" (
  echo [ERROR] Go toolchain not found: %GO%
  echo Please download Go for Windows amd64 and extract to E:\software\go
  pause
  exit /b 1
)

rem 缓存放 E 盘（不写 C 盘）
set "GOCACHE=E:\software\go-cache"
set "GOPATH=E:\software\go-path"
if not exist "%GOCACHE%" mkdir "%GOCACHE%"
if not exist "%GOPATH%" mkdir "%GOPATH%"

rem 产物统一输出到 bin\
if not exist "%ROOT%\bin" mkdir "%ROOT%\bin"

echo [1/3] 构建中… 首次编译约 1-2 分钟
"%GO%" build -trimpath -ldflags "-s -w -H=windowsgui" -o "%ROOT%\bin\workbuddy-hub.exe" .
if not errorlevel 1 goto ok

echo.
echo [ERROR] 构建失败，请查看上面的错误信息。
pause
exit /b 1

:ok
echo [2/3] 构建成功: %ROOT%\bin\workbuddy-hub.exe
echo [3/3] 提示：把 bin\workbuddy-hub.exe 放回 hub-go\ 即可直接替换正在使用的版本。
echo.
pause
