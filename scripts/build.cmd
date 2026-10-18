@echo off
rem ===========================================================================
rem  Launcher for build.ps1 -- bypasses the PowerShell execution policy.
rem  Usage: scripts\build.cmd           (version = dev)
rem         scripts\build.cmd v0.1.0    (explicit version)
rem
rem  NOTE: This file is intentionally ASCII-only. cmd.exe parses batch files
rem  with the OEM/ANSI code page, so UTF-8 non-ASCII characters (e.g. Chinese
rem  comments) corrupt the script. Keep all user-facing Chinese text in the
rem  .ps1 files instead.
rem
rem  -ExecutionPolicy Bypass applies to this run only; it does not change the
rem  system-wide policy.
rem ===========================================================================
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" %*
set "RC=%ERRORLEVEL%"
if not "%RC%"=="0" (
    echo.
    echo [FAILED] build failed with exit code %RC%
)
exit /b %RC%
