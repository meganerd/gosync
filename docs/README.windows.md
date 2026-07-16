# Gosync for Windows

This document provides Windows-specific installation and usage instructions.

## Installation

### Option 1: Download Pre-built Binary

1. Download the latest Windows binary from the [Releases page](https://gitlab.zarquon.space/meganerd/gosync/-/releases)
2. Choose the appropriate architecture:
   - `gosync-windows-amd64.exe` for 64-bit Windows (most common)
   - `gosync-windows-arm64.exe` for ARM-based Windows devices
3. Place the executable in a directory in your PATH (e.g., `C:\Program Files\gosync\`)
4. Open a new Command Prompt or PowerShell window

### Option 2: Build from Source

1. Install Go 1.22 or later from [golang.org](https://golang.org/dl/)
2. Clone the repository:
   ```cmd
   git clone https://gitlab.zarquon.space/meganerd/gosync.git
   cd gosync
   ```
3. Build:
   ```cmd
   go build -o gosync.exe ./cmd/gosync
   ```

### Option 3: Using Make (with Git for Windows)

1. Install [Git for Windows](https://git-scm.com/download/win) which includes Make
2. Open Git Bash
3. Run:
   ```bash
   make build
   ```

## Usage

Gosync works the same way on Windows as on Linux/macOS, but with some Windows-specific considerations:

### Path Separators

Windows uses backslashes (`\`) for paths, but gosync accepts both forward and backward slashes:

```cmd
gosync C:\Users\me\music \\server\share\music
gosync C:/Users/me/music //server/share/music
```

### Network Shares

Gosync can transfer to/from Windows network shares:

```cmd
gosync C:\data \\fileserver\backup
gosync \\fileserver\share C:\local-backup
```

### Running as a Service

To run gosync server as a Windows service, you can use tools like [NSSM](https://nssm.cc/) or [WinSW](https://github.com/winsw/winsw):

#### Using NSSM

1. Download NSSM from https://nssm.cc/download
2. Install gosync service:
   ```cmd
   nssm install GosyncServer "C:\path\to\gosync.exe" "serve --listen :8443"
   nssm set GosyncServer DisplayName "Gosync Transfer Server"
   nssm set GosyncServer Description "High-speed parallel file transfer server"
   nssm set GosyncServer Start SERVICE_AUTO_START
   nssm start GosyncServer
   ```

#### Using WinSW

Create a service configuration file `gosync.xml`:

```xml
<service>
  <id>gosync</id>
  <name>Gosync Transfer Server</name>
  <description>High-speed parallel file transfer server</description>
  <executable>%BASE%\gosync.exe</executable>
  <arguments>serve --listen :8443</arguments>
  <startmode>Automatic</startmode>
</service>
```

Place this alongside `gosync.exe` and use WinSW to manage the service.

### Firewall Configuration

Windows Firewall may block gosync connections. To allow gosync through the firewall:

```powershell
# Allow gosync through Windows Firewall
New-NetFirewallRule -DisplayName "Gosync Server" -Direction Inbound -Program "C:\path\to\gosync.exe" -Action Allow
New-NetFirewallRule -DisplayName "Gosync Client" -Direction Outbound -Program "C:\path\to\gosync.exe" -Action Allow
```

Or allow the specific ports:

```powershell
# Allow TCP port 8443
New-NetFirewallRule -DisplayName "Gosync TCP" -Direction Inbound -LocalPort 8443 -Protocol TCP -Action Allow

# Allow UDP port 8443 (for QUIC)
New-NetFirewallRule -DisplayName "Gosync UDP" -Direction Inbound -LocalPort 8443 -Protocol UDP -Action Allow
```

### Environment Variables

Windows uses different syntax for environment variables:

```cmd
:: Set API key
set GOSYNC_API_KEY=your-api-key-here

:: Or in PowerShell
$env:GOSYNC_API_KEY="your-api-key-here"
```

### Running in Background

Windows doesn't have a direct equivalent to Linux's `&` for backgrounding, but you can use:

```cmd
:: Using start command
start /B gosync serve --listen :8443

:: Using PowerShell
Start-Process -NoNewWindow -FilePath "gosync.exe" -ArgumentList "serve", "--listen", ":8443"
```

## Troubleshooting

### Permission Denied

If you get permission errors, try running Command Prompt or PowerShell as Administrator.

### Port Already in Use

If port 8443 is already in use:

```cmd
:: Find what's using the port
netstat -ano | findstr :8443

:: Kill the process (replace PID with the actual process ID)
taskkill /PID <PID> /F
```

### Antivirus Blocking

Some antivirus software may flag gosync as suspicious. You may need to add an exception for gosync.exe.

### Path Length Limits

Windows has a default maximum path length of 260 characters. If you're transferring deeply nested files, you may need to enable long path support:

1. Open Group Policy Editor (`gpedit.msc`)
2. Navigate to: Computer Configuration → Administrative Templates → System → Filesystem
3. Enable "Enable Win32 long paths"

Or enable via registry:

```reg
Windows Registry Editor Version 5.00

[HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\FileSystem]
"LongPathsEnabled"=dword:00000001
```

## Building Windows Binaries

To build Windows binaries from Linux/macOS:

```bash
# Build for Windows AMD64
GOOS=windows GOARCH=amd64 go build -o gosync-windows-amd64.exe ./cmd/gosync

# Build for Windows ARM64
GOOS=windows GOARCH=arm64 go build -o gosync-windows-arm64.exe ./cmd/gosync

# Or use Make
make dist-windows
```

## Known Limitations

1. **File Permissions**: Windows doesn't support Unix-style file permissions. The `--chmod` flag is ignored on Windows.
2. **Symlinks**: Creating symlinks requires Administrator privileges or Developer Mode enabled.
3. **Case Sensitivity**: Windows filesystems are case-insensitive by default. Be cautious with case-sensitive filenames.
4. **Reserved Names**: Windows reserves certain filenames (CON, PRN, AUX, NUL, COM1-COM9, LPT1-LPT9). Gosync will skip files with these names.
