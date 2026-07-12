@echo off
setlocal

REM ===========================================================================
REM  build\linux\build.bat  <version>
REM
REM  isann-servers LINUX release build, CROSS-COMPILED FROM WINDOWS (GOOS=linux,
REM  CGO_ENABLED=0 -> static ELF). Mirror of build\windows\build.bat. Two
REM  self-contained artifacts under build\linux\out\, market and rendezvous
REM  packaged SEPARATELY:
REM
REM    1) market-linux-amd64.tar.gz
REM         bin\market            the executable (linux/amd64 ELF)
REM         conf\market.json      the config
REM         docker-compose.yaml   the deploy compose
REM    2) rendezvous-linux-amd64.tar.gz
REM         bin\rendezvous        the executable (linux/amd64 ELF)
REM         conf\rendezvous.json  the config
REM         conf\auth.json        RV admission mode (absent = public mode)
REM         docker-compose.yaml   the deploy compose
REM
REM  (market has NO auth.json by design: write access is per-asset signer, not
REM   an operator allow-list -- see pkg/market/config.go.)
REM
REM  Runs on Windows, produces Linux binaries. Packaged as .tar.gz (not .zip):
REM  tar is on every Linux box (no unzip needed) and `ivm rv|market install`
REM  (setup.ExtractTarGz) restores the exec bit on bin\<svc> -- no manual chmod.
REM
REM  Version is injected via ldflags (-X) into pkg/setup.*Version. REQUIRED.
REM
REM    usage:  build\linux\build.bat 0.1.0
REM ===========================================================================

if not "%~1"=="" goto :have_version
echo ERROR: version required ^(no build^).
echo.
echo   usage:  build\linux\build.bat ^<version^>
echo   e.g.    build\linux\build.bat 0.1.0
exit /b 1

:have_version
set "VER=%~1"

REM this script lives in build\linux\  ->  go to repo root
cd /d "%~dp0..\.."

set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0

set "PKG=github.com/isannai/isann-servers/pkg/setup"
set "LDFLAGS=-X %PKG%.MarketVersion=%VER% -X %PKG%.RendezvousVersion=%VER%"

set "OUT=build\linux\out"
set "ARCH=linux-amd64"

if exist "%OUT%" rmdir /s /q "%OUT%"
mkdir "%OUT%"

echo === isann-servers   v%VER%   (linux/amd64, cross from windows) ===
echo.

REM --- market ---------------------------------------------------------------
echo [1/2] market...
set "MKT=%OUT%\market"
mkdir "%MKT%\bin"
mkdir "%MKT%\conf"
go build -trimpath -ldflags "%LDFLAGS%" -o "%MKT%\bin\market" ./cmd/market/
if errorlevel 1 goto :error
copy /Y "deploy\market\conf\*.json" "%MKT%\conf\"    >nul
if errorlevel 1 goto :error
mkdir "%MKT%\docker-compose\market"
copy /Y "docker-compose\market\docker-compose.yaml" "%MKT%\docker-compose\market\" >nul
if errorlevel 1 goto :error
tar -czf "%OUT%\market-%ARCH%.tar.gz" -C "%MKT%" .
if errorlevel 1 goto :error

REM --- rendezvous -----------------------------------------------------------
echo [2/2] rendezvous...
set "RV=%OUT%\rendezvous"
mkdir "%RV%\bin"
mkdir "%RV%\conf"
go build -trimpath -ldflags "%LDFLAGS%" -o "%RV%\bin\rendezvous" ./cmd/rendezvous/
if errorlevel 1 goto :error
copy /Y "deploy\rendezvous\conf\*.json" "%RV%\conf\" >nul
if errorlevel 1 goto :error
mkdir "%RV%\docker-compose\rendezvous"
copy /Y "docker-compose\rendezvous\docker-compose.yaml" "%RV%\docker-compose\rendezvous\" >nul
if errorlevel 1 goto :error
tar -czf "%OUT%\rendezvous-%ARCH%.tar.gz" -C "%RV%" .
if errorlevel 1 goto :error

echo.
echo ===========================================================================
echo  build complete   v%VER%
echo    %OUT%\market-%ARCH%.tar.gz       (+ folder %OUT%\market\)
echo    %OUT%\rendezvous-%ARCH%.tar.gz   (+ folder %OUT%\rendezvous\)
echo ===========================================================================
goto :end

:error
echo.
echo *** BUILD FAILED ***
exit /b 1

:end
endlocal
