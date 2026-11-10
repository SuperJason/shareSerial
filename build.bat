@echo off
REM Builds the windowed binary. -H=windowsgui is required: without it Windows
REM gives the process a console window, so a double-click shows two windows.
setlocal
cd /d "%~dp0"
go build -ldflags "-H=windowsgui -s -w" -o serial-log.exe ./cmd/bridge
if errorlevel 1 (
  echo build failed
  exit /b 1
)
echo built serial-log.exe
