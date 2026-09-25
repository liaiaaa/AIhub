@echo off
rem ============================================================
rem  WorkBuddy Hub (Go) - 启动控制台
rem
rem  默认：所有代理均为停止状态，由你在网页上选择启动哪一个。
rem  控制台地址：http://127.0.0.1:8801
rem
rem  查找顺序（先命中先用）：
rem    1) <项目根>\bin\workbuddy-hub.exe   （build.bat 的默认产物）
rem    2) <项目根>\hub-go\workbuddy-hub.exe （历史位置，兼容旧部署）
rem ============================================================
setlocal
set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "EXE="
if exist "%ROOT%\bin\workbuddy-hub.exe"   set "EXE=%ROOT%\bin\workbuddy-hub.exe"
if not defined EXE if exist "%ROOT%\hub-go\workbuddy-hub.exe" set "EXE=%ROOT%\hub-go\workbuddy-hub.exe"

if not defined EXE (
  echo [ERROR] 未找到 workbuddy-hub.exe
  echo   期望位置之一：
  echo     %ROOT%\bin\workbuddy-hub.exe
  echo     %ROOT%\hub-go\workbuddy-hub.exe
  echo   请先运行 scripts\build.bat 编译。
  pause
  exit /b 1
)

echo 启动: %EXE%
start "" "%EXE%"
