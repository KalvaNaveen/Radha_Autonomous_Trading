@echo off
rem Starts the Radha swing engine and opens the control panel (http://127.0.0.1:8080).
cd /d "%~dp0"
if exist bin\radha-engine.new.exe (
  move /y bin\radha-engine.new.exe bin\radha-engine.exe >nul || (
    echo Could not update bin\radha-engine.exe - close the running engine window and try again.
    pause
    exit /b 1
  )
  echo Engine updated.
)
if not exist bin\radha-engine.exe (
  echo bin\radha-engine.exe not found - run build.bat first.
  pause
  exit /b 1
)
bin\radha-engine.exe %*
pause
