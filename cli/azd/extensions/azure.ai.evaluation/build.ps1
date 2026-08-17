# Ensure script fails on any error
$ErrorActionPreference = 'Stop'

$extensionDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location -Path $extensionDir

$extensionIdSafe = $env:EXTENSION_ID -replace '\.', '-'
$outputDir = if ($env:OUTPUT_DIR) { $env:OUTPUT_DIR } else { Join-Path $extensionDir "bin" }
if (-not (Test-Path -Path $outputDir)) {
    New-Item -ItemType Directory -Path $outputDir | Out-Null
}

$commit = git rev-parse HEAD
if ($LASTEXITCODE -ne 0) {
    Write-Host "Error: Failed to get git commit hash"
    exit 1
}
$buildDate = Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ"

if ($env:EXTENSION_PLATFORM) {
    $platforms = @($env:EXTENSION_PLATFORM)
}
else {
    $platforms = @(
        "windows/amd64",
        "windows/arm64",
        "darwin/amd64",
        "darwin/arm64",
        "linux/amd64",
        "linux/arm64"
    )
}

$versionPath = "$env:EXTENSION_ID/internal/version"
foreach ($platform in $platforms) {
    $os, $arch = $platform -split '/'
    $outputName = Join-Path $outputDir "$extensionIdSafe-$os-$arch"
    if ($os -eq "windows") {
        $outputName += ".exe"
    }

    Write-Host "Building for $os/$arch..."
    if (Test-Path -Path $outputName) {
        Remove-Item -Path $outputName -Force
    }

    $env:GOOS = $os
    $env:GOARCH = $arch
    go build `
        -ldflags="-X '$versionPath.Version=$env:EXTENSION_VERSION' -X '$versionPath.Commit=$commit' -X '$versionPath.BuildDate=$buildDate'" `
        -o $outputName

    if ($LASTEXITCODE -ne 0) {
        Write-Host "An error occurred while building for $os/$arch"
        exit 1
    }
}

Write-Host "Build completed successfully!"
Write-Host "Binaries are located in the $outputDir directory."

