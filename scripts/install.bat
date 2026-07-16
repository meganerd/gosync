@echo off
REM Gosync Installer for Windows
REM Run this script as Administrator

setlocal enabledelayedexpansion

echo ========================================
echo Gosync Installer for Windows
echo ========================================
echo.

REM Check for Administrator privileges
net session >nul 2>&1
if %errorLevel% neq 0 (
    echo ERROR: This script must be run as Administrator
    echo Right-click and select "Run as administrator"
    pause
    exit /b 1
)

REM Determine architecture
if "%PROCESSOR_ARCHITECTURE%"=="ARM64" (
    set ARCH=arm64
) else (
    set ARCH=amd64
)

echo Detected architecture: %ARCH%
echo.

REM Set installation directory
set INSTALL_DIR=C:\Program Files\gosync
set BINARY_NAME=gosync-windows-%ARCH%.exe

REM Create installation directory
if not exist "%INSTALL_DIR%" (
    echo Creating installation directory: %INSTALL_DIR%
    mkdir "%INSTALL_DIR%"
)

REM Download binary
echo Downloading gosync...
echo NOTE: This installer assumes you have the binary downloaded manually.
echo Please download %BINARY_NAME% from:
echo https://gitlab.zarquon.space/meganerd/gosync/-/releases
echo.
echo Place the downloaded file in: %INSTALL_DIR%\gosync.exe
echo.

REM Check if binary exists
if not exist "%INSTALL_DIR%\gosync.exe" (
    echo ERROR: gosync.exe not found in %INSTALL_DIR%
    echo Please download it first and place it in the installation directory.
    pause
    exit /b 1
)

REM Add to PATH
echo Adding to system PATH...
setx PATH "%PATH%;%INSTALL_DIR%" /M >nul 2>&1
if %errorLevel% equ 0 (
    echo ✓ Added to system PATH
) else (
    echo Warning: Could not add to system PATH automatically
    echo Please manually add %INSTALL_DIR% to your PATH
)

echo.
echo ========================================
echo Installation Complete!
echo ========================================
echo.
echo Location: %INSTALL_DIR%\gosync.exe
echo.
echo To use gosync:
echo 1. Open a new Command Prompt or PowerShell window
echo 2. Run: gosync --help
echo.
echo For more information, see docs\README.windows.md
echo.
pause
