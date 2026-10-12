@echo off
rem Zcode Adapter: DSH -> OpenAI(8795) -> official zcode app-server
cd /d "%~dp0"
node adapter.mjs
pause
