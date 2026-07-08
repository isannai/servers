@echo off
setlocal
REM build\windows\build.bat [version] [market^|rendezvous^|all]
REM
REM Build the servers for windows/amd64 into build\out\windows\ — for local
REM dev / verification (run the .exe directly). Linux/docker uses build\linux.
REM
REM   build\windows\build.bat                 both, version "dev"
REM   build\windows\build.bat 0.1.0           both, stamped 0.1.0
REM   build\windows\build.bat 0.1.0 market    just market

set "VER=%~1"
if "%VER%"=="" set "VER=dev"
set "WHAT=%~2"
if "%WHAT%"=="" set "WHAT=all"

REM this script lives in build\windows\ -> repo root
cd /d "%~dp0..\.."

set "PKG=github.com/isannai/isann-servers/pkg/setup"
set "LDFLAGS=-X %PKG%.MarketVersion=%VER% -X %PKG%.RendezvousVersion=%VER%"
set "OUT=build\out\windows"
if not exist "%OUT%" mkdir "%OUT%"

echo === isann-servers build (windows) v%VER% ===

if /I "%WHAT%"=="rendezvous" goto :rv
echo   market      -^> %OUT%\market.exe
go build -ldflags "%LDFLAGS%" -o "%OUT%\market.exe" ./cmd/market
if errorlevel 1 goto :err
if /I "%WHAT%"=="market" goto :done

:rv
echo   rendezvous  -^> %OUT%\rendezvous.exe
go build -ldflags "%LDFLAGS%" -o "%OUT%\rendezvous.exe" ./cmd/rendezvous
if errorlevel 1 goto :err

:done
echo done.
goto :eof

:err
echo *** BUILD FAILED ***
exit /b 1
