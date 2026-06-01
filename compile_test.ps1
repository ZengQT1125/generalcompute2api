$scriptPath = Split-Path -Parent $MyInvocation.MyCommand.Definition
if (!$scriptPath) { $scriptPath = $pwd }

# Resolve absolute paths to bypass encoding issues
$sdkPath = Resolve-Path "$scriptPath\..\go_sdk_new\go"
$env:GOROOT = $sdkPath.Path

# Set isolated GOPATH
$parentPath = (Resolve-Path "$scriptPath\..").Path
$env:GOPATH = "$parentPath\go_path"
$env:GOPROXY = "https://goproxy.cn,direct"
$env:GOTOOLCHAIN = "local"
$env:PATH = "$env:GOROOT\bin;" + $env:PATH

Write-Host "=== Compiling the project ==="
& "$env:GOROOT\bin\go.exe" build -o generalcompute2api.exe ./cmd/generalcompute2api
Write-Host "=== Build Completed Successfully! ==="
