@echo off
cd /d "D:\C\Desktop\workbuddy-hub"
set PYTHONW=E:\software\python\pythonw.exe
if not exist "%PYTHONW%" set PYTHONW=E:\software\python\python.exe
start "" "%PYTHONW%" manager.py
