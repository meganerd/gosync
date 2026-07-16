# Configuration

Gosync can be configured through command-line flags, environment variables, or a JSON configuration file.

## Configuration File

Create a `config.json` file in your current directory or specify a custom path:

```bash
gosync --config /path/to/config.json source destination
```

### Example Configuration

```json
{
  "workers": 8,
  "transport": "tcp",
  "checksum": true,
  "compression": false,
  "progress": true,
  "verbose": false,
  "dry_run": false,
  "exclude": ["*.log", ".git", "__pycache__"],
  "include": ["*.mp3", "*.flac"],
  "server": {
    "listen": ":8443",
    "api_key_env": "GOSYNC_API_KEY"
  },
  "deploy": {
    "enabled": false,
    "target": "",
    "key": ""
  }
}
```

## Configuration Options

### Transfer Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `workers` | int | Auto-detected | Number of parallel transfer workers |
| `transport` | string | "tcp" | Transport backend: "tcp", "quic", "ssh" |
| `checksum` | bool | false | Enable SHA256 checksum verification |
| `compression` | bool | false | Enable gzip compression |
| `progress` | bool | false | Show progress bar |
| `verbose` | bool | false | Enable verbose logging |
| `dry_run` | bool | false | Show what would be transferred without transferring |

### Filter Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `exclude` | []string | [] | Patterns to exclude (glob syntax) |
| `include` | []string | [] | Patterns to include (glob syntax) |

### Server Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `server.listen` | string | ":8443" | Address and port to listen on |
| `server.api_key_env` | string | "GOSYNC_API_KEY" | Environment variable for API key |

### Deploy Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `deploy.enabled` | bool | false | Enable deployment mode |
| `deploy.target` | string | "" | Target host for deployment |
| `deploy.key` | string | "" | SSH key for deployment |

## Command-Line Flags

All configuration options can be set via command-line flags:

```bash
gosync --workers 8 --transport quic --checksum --progress source destination
```

### Flag Priority

Command-line flags override configuration file values, which override default values.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `GOSYNC_API_KEY` | API key for authenticated transfers |
| `GOSYNC_WORKERS` | Number of parallel workers |
| `GOSYNC_TRANSPORT` | Transport backend |
| `GOSYNC_CHECKSUM` | Enable checksum verification |
| `GOSYNC_COMPRESSION` | Enable compression |
| `GOSYNC_PROGRESS` | Show progress |
| `GOSYNC_VERBOSE` | Enable verbose logging |

## Platform-Specific Configuration

### Linux

Gosync works out of the box on Linux. For optimal performance:

```bash
# Tune network buffers
sudo sysctl -w net.core.rmem_max=16777216
sudo sysctl -w net.core.wmem_max=16777216

# Increase file descriptor limits
ulimit -n 65536
```

### macOS

On macOS, you may need to increase the maximum number of open files:

```bash
ulimit -n 10240
```

### Windows

See [Windows Documentation](README.windows.md) for Windows-specific configuration.

## Example Configurations

### High-Speed Network (40 Gbps+)

```json
{
  "workers": 16,
  "transport": "quic",
  "checksum": true,
  "compression": false,
  "progress": true,
  "verbose": false
}
```

### Conservative Transfer

```json
{
  "workers": 4,
  "transport": "tcp",
  "checksum": true,
  "compression": true,
  "progress": true,
  "verbose": false
}
```

### Development/Testing

```json
{
  "workers": 2,
  "transport": "tcp",
  "checksum": false,
  "compression": false,
  "progress": true,
  "verbose": true,
  "dry_run": true
}
```

## Configuration Validation

Gosync validates configuration on startup. If you provide invalid values, it will:

1. Print an error message
2. Show the problematic option
3. Exit with a non-zero exit code

Example:

```bash
$ gosync --workers -1 source destination
Error: workers must be a positive integer
```

## Debugging Configuration

To see the effective configuration:

```bash
gosync --verbose source destination
```

This will print the resolved configuration values at startup.
