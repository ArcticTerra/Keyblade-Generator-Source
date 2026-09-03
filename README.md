# KH1 Keyblade Generator
Source code for KH1 Keyblade Generator v1.1.8.

This repository is provided as the source corresponding to the compiled Windows executable distributed with the KH1 Keyblade Generator.

## Building and testing

### Requirements

- Windows
- Go for Windows: https://go.dev/dl/

### Required folder structure

The application uses the following folder structure:

```text
Keyblade Generator Source\
│
├─ data\
│  └─ keyblades.json
│
└─ source\
   └─ main.go
```

### Build command

Open Command Prompt in the directory containing `main.go` and run:

```bat
go build -trimpath -ldflags="-H windowsgui" -o "..\KH1 Keyblade Generator.exe" main.go
```

## Runtime files

The release package includes additional data files used by the application, such as `data/keyblades.json`. These files are distributed alongside the executable and are not part of the compiled Go source.
