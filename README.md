# coredns-config-adapter

This project provides a dynamic adapter for CoreDNS configuration files. It watches a directory for `.server` files (such as those mounted from Kubernetes ConfigMaps), processes them, and generates a unified CoreDNS configuration file with custom bind statements.

## Features

- **Automatic Directory Watching:** Monitors a directory for creation, modification, and deletion of `.server` files.
- **Kubernetes-Ready:** Handles atomic symlink swaps as performed by Kubernetes ConfigMap and Secret mounts.
- **Custom Bind Statement:** Inserts a configurable `bind` statement into each server block.
- **Automatic Regeneration:** Rewrites the output configuration file whenever a relevant change is detected.

## Usage

### Build

```sh
go build -o coredns-config-adapter
```

### Run

```sh
./coredns-config-adapter \
  -inputDir /etc/custom \
  -outputDir /etc/generated-config \
  -bind "bind 169.254.20.10 10.255.128.10"
```

- `-inputDir`: Directory containing `.server` files (default: `/etc/custom`)
- `-outputDir`: Directory to write the generated config file (default: `/etc/generated-config`)
- `-bind`: Bind statement to insert into each server block

### Example

Suppose you have a Kubernetes ConfigMap mounted at `/etc/custom` with files like `foo.server`. The adapter will watch this directory and automatically update `/etc/generated-config/custom-server-block.override` whenever any `.server` file changes.

## How It Works

1. **Startup:**  
   - Watches the specified input directory for file changes.
   - Performs an initial config generation.

2. **On Change:**  
   - When a `.server` file is created, modified, or deleted, the adapter:
     - Reads all `.server` files in the directory.
     - Parses and processes each file, inserting the custom bind statement.
     - Writes the combined configuration to the output file.

3. **Kubernetes Compatibility:**  
   - Handles symlink swaps and atomic updates typical of Kubernetes ConfigMap/Secret mounts.

## Notes

- Only files ending with `.server` are processed.
- The output file is always named `custom-server-block.override` in the output directory.
- The `coredns-conig-adapter` is designed to run as a sidecar or helper in Kubernetes environments.