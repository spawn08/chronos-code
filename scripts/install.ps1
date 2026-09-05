#!/usr/bin/env pwsh
# Installs the chronos-code binary from a GitHub Release.
#
# Usage:
#   irm https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.ps1 | iex
#
# Env overrides:
#   $env:VERSION     release tag to install, e.g. v1.2.3 (default: latest)
#   $env:INSTALL_DIR directory to install the binary into (default: $HOME\.local\bin)

$ErrorActionPreference = "Stop"

$Repo = "spawn08/chronos-code"
$Version = if ($env:VERSION) { $env:VERSION } else { "latest" }
$InstallDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $HOME ".local\bin" }

$arch = if ([Environment]::Is64BitOperatingSystem) {
  if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
} else {
  throw "error: unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
}

if ($Version -eq "latest") {
  $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
  $tag = $release.tag_name
} else {
  $tag = $Version
}

$archive = "chronos-code-$tag-windows-$arch.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$tag"

$workdir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $workdir | Out-Null
try {
  Write-Host "Downloading $archive ($tag)..."
  $archivePath = Join-Path $workdir $archive
  Invoke-WebRequest -Uri "$baseUrl/$archive" -OutFile $archivePath
  $checksumsPath = Join-Path $workdir "checksums-sha256.txt"
  Invoke-WebRequest -Uri "$baseUrl/checksums-sha256.txt" -OutFile $checksumsPath

  Write-Host "Verifying checksum..."
  $expected = (Select-String -Path $checksumsPath -Pattern " $archive$").Line.Split(" ")[0]
  $actual = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()
  if ($expected -ne $actual) {
    throw "checksum mismatch for $archive"
  }

  Write-Host "Installing to $InstallDir..."
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  Expand-Archive -Path $archivePath -DestinationPath $workdir -Force
  Copy-Item -Path (Join-Path $workdir "chronos-code.exe") -Destination (Join-Path $InstallDir "chronos-code.exe") -Force

  Write-Host "Installed chronos-code $tag to $InstallDir\chronos-code.exe"
  if (($env:Path -split ";") -notcontains $InstallDir) {
    Write-Host "Note: $InstallDir is not on your PATH. Add it, e.g.: `$env:Path += `";$InstallDir`""
  }
} finally {
  Remove-Item -Recurse -Force $workdir
}
