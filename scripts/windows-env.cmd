@echo off
rem Toolchain setup for scripts/windows-test.sh: copy this file to the work
rem directory on the Windows machine as env.cmd (or name it with
rem WINDOWS_ENV). It is `call`ed from that directory before every command
rem and keeps everything Go writes under the directory it lives in (module
rem and build caches, telemetry counters, the tests' temporary files):
rem
rem   go\        a Go zip unpacked here (or leave it out and use a Go on the
rem              machine's PATH)
rem   mingw64\   MinGW-w64, e.g. MSYS2's mingw64, for the tests module's cgo
rem              reference and the C oracles
rem   tmp\       the tests' scratch: test environments are full map-size
rem              files on Windows, so delete them between sessions, or point
rem              TEMP and TMP at a drive with room (the tests' temporary
rem              directories follow them)
rem   src\       the checkout, sent by windows-test.sh
rem
rem The first run downloads golang.org/x/sys through the Go module proxy
rem into %GOPATH%\pkg\mod.
set "GOPATH=%~dp0gopath"
set "GOCACHE=%~dp0gocache"
set GOENV=off
set GOTOOLCHAIN=local
set "APPDATA=%~dp0appdata"
if not exist "%~dp0tmp" mkdir "%~dp0tmp"
set "TMP=%~dp0tmp"
set "TEMP=%~dp0tmp"
set CGO_ENABLED=1
set CC=gcc
set "PATH=%~dp0go\bin;%~dp0mingw64\bin;%PATH%"
