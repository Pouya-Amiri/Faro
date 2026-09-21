param(
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$')]
    [string]$Version
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$distDir = Join-Path $repoRoot 'dist_actions'
$stageRoot = Join-Path $repoRoot 'build\package'
$clientStage = Join-Path $stageRoot 'windows-client'
$serverStage = Join-Path $stageRoot 'windows-server'

foreach ($stage in @($clientStage, $serverStage)) {
    if (Test-Path $stage) {
        Remove-Item -Recurse -Force $stage
    }
    New-Item -ItemType Directory -Force -Path $stage | Out-Null
}
New-Item -ItemType Directory -Force -Path $distDir | Out-Null

Copy-Item (Join-Path $repoRoot 'build\bin\Faro.exe') $clientStage
Copy-Item (Join-Path $repoRoot 'build\bin\faro-server.exe') $serverStage
foreach ($notice in @('LICENSE', 'THIRD_PARTY_NOTICES.txt')) {
    Copy-Item (Join-Path $repoRoot $notice) $clientStage
    Copy-Item (Join-Path $repoRoot $notice) $serverStage
}

$clientArchive = Join-Path $distDir "Faro-$Version-windows-x86_64-portable.zip"
$serverArchive = Join-Path $distDir "faro-server-$Version-windows-x86_64.zip"
Remove-Item -Force -ErrorAction SilentlyContinue $clientArchive, $serverArchive
Compress-Archive -Path (Join-Path $clientStage '*') -DestinationPath $clientArchive -CompressionLevel Optimal
Compress-Archive -Path (Join-Path $serverStage '*') -DestinationPath $serverArchive -CompressionLevel Optimal
