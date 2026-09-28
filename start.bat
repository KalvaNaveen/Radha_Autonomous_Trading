@echo off
rem Starts the Radha swing engine and opens the control panel (http://127.0.0.1:8080).
cd /d "%~dp0"
if not exist bin\radha-engine.exe (
  echo bin\radha-engine.exe not found - run build.bat first.
  pause
  exit /b 1
)
bin\radha-engine.exe %*
pause
