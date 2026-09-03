# KH1 Keyblade Generator
Source code for KH1 Keyblade Generator v1.1.8.

This repository is provided as the source corresponding to the compiled Windows executable distributed with the KH1 Keyblade Generator.

## Building

### Requirements

- Windows
- Go for Windows: https://go.dev/dl/

### Build command

Open Command Prompt in the directory containing `main.go` and run:

```bat
go build -trimpath -ldflags="-H windowsgui" -o "KH1 Keyblade Generator.exe" main.go
