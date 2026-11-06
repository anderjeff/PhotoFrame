# Builds the frame binary locally and installs it on the Pi over SSH, for fast
# iteration without cutting a release. Requires the Windows OpenSSH client.
#
#   .\scripts\deploy-frame.ps1 -PiHost 192.168.1.50 -User pi
#   .\scripts\deploy-frame.ps1 -PiHost photoframe.local -Arch arm64
param(
    [Parameter(Mandatory = $true)][string]$PiHost,
    [string]$User = "pi",
    [ValidateSet("armv7", "arm64")][string]$Arch = "armv7"
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$out = Join-Path $root "dist\photoframe-linux-$Arch"
$version = "dev-" + (git -C $root rev-parse --short HEAD 2>$null)

$env:CGO_ENABLED = "0"; $env:GOOS = "linux"
if ($Arch -eq "armv7") { $env:GOARCH = "arm"; $env:GOARM = "7" } else { $env:GOARCH = "arm64"; Remove-Item Env:GOARM -ErrorAction SilentlyContinue }
try {
    Write-Host "Building $out ($version)"
    go build -C $root -trimpath -ldflags "-s -w -X photoframe/internal/version.Version=$version" -o $out ./cmd/frame
    if ($LASTEXITCODE) { throw "build failed" }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:GOARM, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

$target = "$User@$PiHost"
Write-Host "Copying to $target"
scp $out (Join-Path $root "scripts\install-frame.sh") "${target}:/tmp/"
if ($LASTEXITCODE) { throw "scp failed" }
ssh -t $target "sed -i 's/\r$//' /tmp/install-frame.sh && sudo bash /tmp/install-frame.sh --binary /tmp/photoframe-linux-$Arch"
