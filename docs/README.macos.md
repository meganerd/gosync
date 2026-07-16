# Gosync for macOS

This document provides macOS-specific installation and usage instructions.

## Installation

### Option 1: Using Homebrew (Recommended)

```bash
# Add the tap (when available)
brew tap meganerd/gosync

# Install gosync
brew install gosync
```

### Option 2: Download Pre-built Binary

1. Download the latest macOS binary from the [Releases page](https://gitlab.zarquon.space/meganerd/gosync/-/releases)
2. Choose the appropriate architecture:
   - `gosync-darwin-amd64` for Intel Macs
   - `gosync-darwin-arm64` for Apple Silicon (M1/M2/M3)
3. Make the binary executable and move to PATH:

```bash
chmod +x gosync-darwin-*
mv gosync-darwin-* /usr/local/bin/gosync
```

### Option 3: Build from Source

1. Install Xcode Command Line Tools:
   ```bash
   xcode-select --install
   ```

2. Install Go using Homebrew:
   ```bash
   brew install go
   ```

3. Clone and build:
   ```bash
   git clone https://gitlab.zarquon.space/meganerd/gosync.git
   cd gosync
   make build
   ```

### Option 4: Using Make

```bash
# Build for current architecture
make build

# Build for specific architecture
GOOS=darwin GOARCH=arm64 make build  # Apple Silicon
GOOS=darwin GOARCH=amd64 make build  # Intel
```

## Usage

Gosync works the same way on macOS as on Linux, with some macOS-specific considerations:

### File Permissions

macOS uses BSD-style permissions, similar to Linux:

```bash
# Transfer with permission preservation
gosync --preserve-perms /source /destination

# Transfer without permission preservation (default)
gosync /source /destination
```

### Resource Forks and Metadata

macOS uses extended attributes and resource forks. By default, gosync doesn't preserve these:

```bash
# To preserve macOS metadata (experimental)
gosync --preserve-xattrs /source /destination
```

### Case Sensitivity

macOS filesystems are case-insensitive by default (HFS+ and APFS). Be cautious with case-sensitive filenames:

```bash
# These may overwrite each other on macOS
gosync File.txt file.txt /destination
```

### Spotlight Indexing

Spotlight may slow down transfers. You can exclude the destination from Spotlight:

```bash
# Add to .metadata_never_index (create this file in destination)
touch /destination/.metadata_never_index
```

### Time Machine

Time Machine may interfere with large transfers. Consider pausing it during major transfers:

```bash
# Disable Time Machine temporarily
sudo tmutil disable

# Re-enable when done
sudo tmutil enable
```

## Performance Tuning

### Network Optimization

macOS has good network defaults, but you can optimize further:

```bash
# Increase socket buffer sizes
sudo sysctl -w net.inet.tcp.recvspace=262144
sudo sysctl -w net.inet.tcp.sendspace=262144

# Increase max connections
sudo sysctl -w kern.maxfiles=65536
sudo sysctl -w kern.maxfilesperproc=65536
```

### File System

For best performance with large files:

```bash
# Use APFS (default on modern macOS)
# Enable TRIM for SSDs (usually automatic)
sudo trimforce enable
```

### Memory Management

macOS manages memory aggressively. For large transfers:

```bash
# Monitor memory usage
memory_pressure

# Check swap usage
sysctl vm.swapusage
```

## Integration with macOS

### Finder Integration

Create a Droplet for easy drag-and-drop transfers:

1. Open Automator
2. Choose "Application"
3. Add "Run Shell Script" action
4. Enter: `/usr/local/bin/gosync "$@" /path/to/destination`
5. Save as "Gosync Droplet.app"

### Spotlight Integration

Add gosync transfers to Spotlight comments:

```bash
# After transfer, add comment
mdimport /destination
```

### Notification Center

Get notifications when transfers complete (using `osascript`):

```bash
gosync /source /destination && osascript -e 'display notification "Transfer complete" with title "Gosync"'
```

## Troubleshooting

### "Permission denied" Errors

macOS may block unsigned applications:

```bash
# Remove quarantine attribute
xattr -d com.apple.quarantine /usr/local/bin/gosync

# Or allow in System Preferences > Security & Privacy
```

### Port Already in Use

```bash
# Find what's using the port
lsof -i :8443

# Kill the process (replace PID)
kill -9 <PID>
```

### Firewall Issues

macOS firewall may block connections:

1. Go to System Preferences > Security & Privacy > Firewall
2. Click the lock to make changes
3. Click "Firewall Options"
4. Add gosync or allow incoming connections

### File System Events

macOS's FSEvents may slow down directory scanning:

```bash
# Disable FSEvents for specific directories
sudo mdutil -i off /destination
```

### Spotlight Indexing

If Spotlight is slowing transfers:

```bash
# Check if directory is indexed
mdls /destination

# Exclude from Spotlight
sudo mdutil -i off /destination
```

## Building macOS Binaries

To build macOS binaries from Linux:

```bash
# Build for macOS AMD64 (Intel)
GOOS=darwin GOARCH=amd64 go build -o gosync-darwin-amd64 ./cmd/gosync

# Build for macOS ARM64 (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o gosync-darwin-arm64 ./cmd/gosync

# Or use Make
make dist-darwin
```

## Known Limitations

1. **File Locks**: macOS uses advisory file locks. Gosync may fail if files are locked by other processes.
2. **Extended Attributes**: By default, gosync doesn't preserve macOS extended attributes.
3. **Resource Forks**: Resource forks are not preserved by default.
4. **Time Machine**: Time Machine may interfere with large transfers.
5. **Spotlight**: Spotlight indexing can slow down directory scanning.
6. **App Nap**: macOS may reduce gosync's priority when running in background. Disable in Activity Monitor if needed.

## macOS-Specific Features

### AirDrop Integration

Gosync doesn't directly integrate with AirDrop, but you can use it to transfer to/from the AirDrop directory:

```bash
# Transfer to AirDrop directory (for sharing)
gosync /source ~/Library/Messages/Attachments/
```

### iCloud Drive

Transfer to/from iCloud Drive:

```bash
# Transfer to iCloud Drive
gosync /source ~/Library/Mobile\ Documents/com~apple~CloudDocs/

# Transfer from iCloud Drive
gosync ~/Library/Mobile\ Documents/com~apple~CloudDocs/ /destination
```

### Network Extension

For advanced network configurations, consider using macOS's network extension framework (requires developer account).
