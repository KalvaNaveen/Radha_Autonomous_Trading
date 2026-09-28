@echo off
rem Runs a 5-year backtest from the command line (needs today's Kite login from the control panel).
cd /d "%~dp0"
bin\radha-backtest.exe -years 5 %*
start "" backtest-report\report.html
pause
