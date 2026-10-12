@echo off
rem ============================================================
rem  AIHub (Go) - start console (no rebuild)
rem
rem  Opens the console at http://127.0.0.1:8801
rem  All proxies start STOPPED; pick one on the web page.
rem
rem  Locates the exe in this order:
rem    1) <root>\bin\aihub.exe
rem    2) <root>\hub-go\aihub.exe   (legacy location)
rem
rem  ASCII-only + CRLF on purpose (see build.bat notes).
rem ============================================================
setlocal
set "ROOT=%~dp0.."
for %%I in ("%ROOT%") do set "ROOT=%%~fI"

set "EXE="
if exist "%ROOT%\bin\aihub.exe" set "EXE=%ROOT%\bin\aihub.exe"
if not defined EXE if exist "%ROOT%\hub-go\aihub.exe" set "EXE=%ROOT%\hub-go\aihub.exe"

if not defined EXE (
  echo [ERROR] aihub.exe not found. Expected one of:
  echo     %ROOT%\bin\aihub.exe
  echo     %ROOT%\hub-go\aihub.exe
  echo Run scripts\build.bat first.
  pause
  exit /b 1
)

echo Starting: %EXE%
start "" "%EXE%"