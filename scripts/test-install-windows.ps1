# Native Windows acceptance: real binary/notices, offline downloads and legacy fallback.
$ErrorActionPreference = 'Stop'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('tutor-installer-test-' + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $testRoot | Out-Null
$previousCGO = $env:CGO_ENABLED
try {
    $env:CGO_ENABLED = '0'
    $payload = Join-Path $testRoot 'payload'
    New-Item -ItemType Directory -Path $payload | Out-Null
    $binary = Join-Path $payload 'tutor-mcp.exe'
    go build -o $binary .
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    $notices = Join-Path $payload 'THIRD_PARTY_NOTICES'
    python -B "$PSScriptRoot/third_party_notices.py" --binary $binary --output $notices
    if ($LASTEXITCODE -ne 0) { throw 'Notice collection failed' }
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot '../LICENSE') -Destination (Join-Path $payload 'LICENSE')
    $arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() -eq 'Arm64') { 'arm64' } else { 'amd64' }
    $asset = "tutor-mcp_windows_$arch.zip"
    $archive = Join-Path $testRoot $asset
    function Write-MainArchive([switch]$Legacy) {
        $files = @($binary, (Join-Path $payload 'LICENSE'))
        if (-not $Legacy) { $files += $notices }
        Compress-Archive -LiteralPath $files -DestinationPath $archive -Force
        "$((Get-FileHash -Algorithm SHA256 $archive).Hash)  $asset" | Set-Content (Join-Path $testRoot 'SHA256SUMS') -Encoding ascii
    }
    function Invoke-WebRequest {
        param([switch]$UseBasicParsing, [string]$Uri, [string]$OutFile)
        Copy-Item -LiteralPath (Join-Path $testRoot ($Uri.Split('/')[-1])) -Destination $OutFile
    }
    $destination = Join-Path $testRoot 'bin with spaces'
    function Assert-Rejected([scriptblock]$Action, [string]$Message) {
        $rejected = $false
        try { & $Action }
        catch { if ($_.Exception.Message -notmatch $Message) { throw }; $rejected = $true }
        if (-not $rejected) { throw "Invalid distribution accepted: $Message" }
        if ((Get-FileHash (Join-Path $destination 'tutor-mcp.exe')).Hash -ne $script:installedBinaryHash) {
            throw 'Failed update changed installed binary'
        }
        if ((Get-FileHash $script:installedGoLicense).Hash -ne $script:installedLicenseHash) {
            throw 'Failed update changed installed notices'
        }
    }
    Write-MainArchive
    & "$PSScriptRoot/install.ps1" -Version v0.6.1 -InstallDir $destination -NoPathUpdate
    & (Join-Path $destination 'tutor-mcp.exe') --version
    if ($LASTEXITCODE -ne 0) { throw 'Installed binary failed' }
    $inventoryPath = Join-Path $notices 'inventory.json'
    $originalInventory = Get-Content -LiteralPath $inventoryPath -Raw
    $inventory = $originalInventory | ConvertFrom-Json
    $goNotice = @($inventory.components | Where-Object { $_.path -eq 'go' })[0].notices[0].file
    $script:installedBinaryHash = (Get-FileHash (Join-Path $destination 'tutor-mcp.exe')).Hash
    $script:installedGoLicense = Join-Path (Join-Path $destination 'tutor-mcp-notices/THIRD_PARTY_NOTICES') $goNotice
    $script:installedLicenseHash = (Get-FileHash $script:installedGoLicense).Hash
    if ($script:installedLicenseHash -ne (Get-FileHash (Join-Path $notices $goNotice)).Hash) { throw 'Installed notice differs' }
    "$('0' * 64)  $asset" | Set-Content (Join-Path $testRoot 'SHA256SUMS') -Encoding ascii
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version v0.6.1 -InstallDir $destination -NoPathUpdate } 'Checksum mismatch'
    $goNoticePath = Join-Path $notices $goNotice
    $goNoticeBytes = [IO.File]::ReadAllBytes($goNoticePath)
    'Changed notice' | Set-Content -LiteralPath $goNoticePath
    Write-MainArchive
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version v0.6.1 -InstallDir $destination -NoPathUpdate } 'Notice file checksum mismatch'
    Remove-Item -LiteralPath $goNoticePath
    Write-MainArchive
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version v0.6.1 -InstallDir $destination -NoPathUpdate } 'Missing notice file'
    [IO.File]::WriteAllBytes($goNoticePath, $goNoticeBytes)

    # A legacy binary archive is unchanged; notices are separate, hash-verified assets.
    Write-MainArchive -Legacy
    $supplementRoot = Join-Path $testRoot 'supplement-payload'
    New-Item -ItemType Directory -Path $supplementRoot | Out-Null
    Copy-Item -LiteralPath $notices -Destination $supplementRoot -Recurse
    Copy-Item -LiteralPath (Join-Path $payload 'LICENSE') -Destination $supplementRoot
    $supplementInventoryPath = Join-Path $supplementRoot 'THIRD_PARTY_NOTICES/inventory.json'
    $supplementInventory = $originalInventory | ConvertFrom-Json
    $supplementInventory | Add-Member -NotePropertyName release_version -NotePropertyValue 'v0.6.1'
    $supplementInventory | Add-Member -NotePropertyName source_commit -NotePropertyValue '440f837e7f043f2f64db23be9459512375dfa542'
    $supplementInventory | Add-Member -NotePropertyName original_archive_sha256 -NotePropertyValue (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
    $supplementName = "tutor-mcp_windows_$($arch)_notices.zip"
    $supplement = Join-Path $testRoot $supplementName
    function Write-Supplement {
        $supplementInventory | ConvertTo-Json -Depth 100 | Set-Content -LiteralPath $supplementInventoryPath -Encoding utf8
        Compress-Archive -LiteralPath @((Join-Path $supplementRoot 'LICENSE'), (Join-Path $supplementRoot 'THIRD_PARTY_NOTICES')) -DestinationPath $supplement -Force
        "$((Get-FileHash -Algorithm SHA256 $supplement).Hash)  $supplementName" | Set-Content (Join-Path $testRoot 'THIRD_PARTY_NOTICES_SHA256SUMS') -Encoding ascii
    }
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version latest -InstallDir $destination -NoPathUpdate } 'Cannot find path|does not exist'
    Write-Supplement
    foreach ($version in @('latest', 'v0.6.1')) {
        & "$PSScriptRoot/install.ps1" -Version $version -InstallDir $destination -NoPathUpdate
        if ((Get-FileHash $script:installedGoLicense).Hash -ne $script:installedLicenseHash) { throw 'Legacy installation lost notice' }
    }
    "$('0' * 64)  $supplementName" | Set-Content (Join-Path $testRoot 'THIRD_PARTY_NOTICES_SHA256SUMS') -Encoding ascii
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version latest -InstallDir $destination -NoPathUpdate } 'Checksum mismatch'
    $supplementInventory.binary_sha256 = '0' * 64
    Write-Supplement
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version latest -InstallDir $destination -NoPathUpdate } 'Notice inventory binary checksum mismatch'
    $supplementInventory.binary_sha256 = $inventory.binary_sha256
    $supplementInventory.original_archive_sha256 = '0' * 64
    Write-Supplement
    Assert-Rejected { & "$PSScriptRoot/install.ps1" -Version latest -InstallDir $destination -NoPathUpdate } 'Legacy notice supplement release or archive checksum mismatch'
} finally {
    $env:CGO_ENABLED = $previousCGO
    Remove-Item -LiteralPath $testRoot -Recurse -Force
}
