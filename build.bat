@echo off
rem Builds the Windows binaries from source (requires Go 1.22+: https://go.dev/dl).
cd /d "%~dp0"
go mod tidy || goto :err
go test ./... || goto :err
go build -trimpath -o bin\radha-engine.exe .\cmd\engine || goto :err
go build -trimpath -o bin\radha-backtest.exe .\cmd\backtest || goto :err
go build -trimpath -o bin\kitemock.exe .\tools\kitemock || goto :err
echo Build OK.
pause
exit /b 0
:err
echo Build FAILED.
pause
exit /b 1
