@echo off
rem ===========================================================================
rem  Opens a PowerShell window with the local development environment ready.
rem  Usage: double-click scripts\dev-shell.cmd
rem
rem  NOTE: ASCII-only on purpose (see build.cmd for the reason).
rem  Environment variables set here live only in that window.
rem ===========================================================================
setlocal
cd /d "%~dp0.."
powershell.exe -NoProfile -ExecutionPolicy Bypass -NoExit -Command ". '%~dp0dev-env.ps1'"
