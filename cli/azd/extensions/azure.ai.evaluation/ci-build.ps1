param(
    [string] $Version = (Get-Content "$PSScriptRoot/version.txt"),
    [string] $SourceVersion = (git rev-parse HEAD),
    [switch] $CodeCoverageEnabled,
    [switch] $BuildRecordMode,
    [string] $MSYS2Shell,
    [string] $OutputFileName
)

$PSNativeCommandArgumentPassing = 'Legacy'

go clean
if ($LASTEXITCODE) {
    Write-Host "Error running go clean"
    exit $LASTEXITCODE
}

$buildFlags = @("-trimpath", "-buildmode=pie")
if ($CodeCoverageEnabled) {
    $buildFlags += "-cover"
}

$buildFlags += @(
    "-tags=cfi,cfg,osusergo",
    "-ldflags=-s -w -X azure.ai.evaluation/internal/version.Version=$Version -X azure.ai.evaluation/internal/version.Commit=$SourceVersion -X azure.ai.evaluation/internal/version.BuildDate=$(Get-Date -Format o) ",
    "-o=$OutputFileName"
)

go build @buildFlags
if ($LASTEXITCODE) {
    Write-Host "Error running go build"
    exit $LASTEXITCODE
}

Write-Host "go build succeeded"

