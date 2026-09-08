@echo off
setlocal
rem Start/stop the local Goobers dashboard monitor.
rem
rem   monitor start     begin watching in the background
rem   monitor stop      stop watching
rem   monitor state     is it running, and how many findings are open
rem   monitor status    list tracked findings
rem
rem Unix equivalent: ./monitor (same subcommands).

set "CLI=%~dp0tools\uiwatch\uiwatch.mjs"

where node >nul 2>nul
if errorlevel 1 (
  echo monitor: node is required but was not found on PATH 1>&2
  exit /b 1
)
if not exist "%CLI%" (
  echo monitor: cannot find "%CLI%" 1>&2
  exit /b 1
)

if "%~1"=="" (
  node "%CLI%" state
) else (
  node "%CLI%" %*
)
exit /b %errorlevel%
