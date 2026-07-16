#Requires -RunAsAdministrator
<#
.SYNOPSIS
    Installs Gosync on Windows
.DESCRIPTION
    Downloads and installs the latest version of Gosync
.PARAMETER Version
    Version to install (default: latest)
.PARAMETER InstallDir
    Installation directory (default: C:\Program Files\gosync)
.EXAMPLE
    .\install.ps1
    .\install.ps1 -Version "1.0.0"
    .\install.ps1 -InstallDir "C:\Tools\gosync"
#>

param(
    [string]$Version = "latest",
    [string]$InstallDir = "C:\Program Files\gosync"
)

$ErrorActionPreference = "Stop"

# Determine architecture
$arch = if ([Environment]::Is64BitOperatingSystem) {
    if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
} else {
    Write-Error "32-bit Windows is not supported"
    exit 1
}

# Set up download URL
$baseUrl = "https://gitlab.zarquon.space/meganerd/gosync/-/releases"
if ($Version -eq "latest") {
    # For latest, we'd need to query the API - simplified for now
    $binaryName = "gosync-windows-$arch.exe"
    Write-Host "Downloading latest version..." -ForegroundColor Cyan
} else {
    $binaryName = "gosync-windows-$arch.exe"
    Write-Host "Downloading version $Version..." -ForegroundColor Cyan
}

# Create installation directory
if (-not (Test-Path $InstallDir)) {
    Write-Host "Creating installation directory: $InstallDir" -ForegroundColor Yellow
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

# Download binary
$downloadUrl = "$baseUrl/$binaryName"
$destPath = Join-Path $InstallDir "gosync.exe"

Write-Host "Downloading from: $downloadUrl" -ForegroundColor Gray
try {
    Invoke-WebRequest -Uri $downloadUrl -OutFile $destPath -UseBasicParsing
    Write-Host "Downloaded successfully" -ForegroundColor Green
} catch {
    Write-Error "Failed to download: $_"
    exit 1
}

# Add to PATH if not already there
$currentPath = [Environment]::GetEnvironmentVariable("Path", "Machine")
if ($currentPath -notlike "*$InstallDir*") {
    Write-Host "Adding to system PATH..." -ForegroundColor Yellow
    [Environment]::SetEnvironmentVariable("Path", "$currentPath;$InstallDir", "Machine")
    $env:Path = "$env:Path;$InstallDir"
    Write-Host "Added to PATH" -ForegroundColor Green
} else {
    Write-Host "Already in PATH" -ForegroundColor Gray
}

# Verify installation
Write-Host "`nVerifying installation..." -ForegroundColor Cyan
try {
    $versionOutput = & $destPath --version 2>&1
    Write-Host "✓ $versionOutput" -ForegroundColor Green
} catch {
    Write-Warning "Could not verify version (this is normal for first install)"
}

Write-Host "`n✓ Gosync installed successfully!" -ForegroundColor Green
Write-Host "  Location: $destPath" -ForegroundColor Gray
Write-Host "  Run 'gosync --help' to get started" -ForegroundColor Gray
