# Install TUTOR MCP for the current Windows user; no administrator rights needed.
[CmdletBinding()]
param(
    [string]$Version = $(if ($env:TUTOR_MCP_VERSION) { $env:TUTOR_MCP_VERSION } else { 'latest' }),
    [string]$InstallDir = $(if ($env:TUTOR_MCP_INSTALL_DIR) { $env:TUTOR_MCP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tutor-mcp' }),
    [switch]$NoPathUpdate
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Assert-ArchiveChecksum([string]$Archive, [string]$Sums, [string]$Name) {
    $entries = @(Get-Content $Sums | Where-Object { $_ -match ('^[a-fA-F0-9]{64}\s+\*?' + [regex]::Escape($Name) + '$') })
    if ($entries.Count -ne 1) { throw "Missing or ambiguous checksum for $Name" }
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $Archive).Hash -ne ($entries[0] -split '\s+')[0]) {
        throw "Checksum mismatch for $Name"
    }
}

function Assert-NoticeInventory([string]$Notices, [string]$Binary, [string]$ExpectedArch) {
    $inventory = Get-Content -LiteralPath (Join-Path $Notices 'inventory.json') -Raw | ConvertFrom-Json
    if ($inventory.format_version -ne 1 -or $inventory.binary -ne 'tutor-mcp.exe' -or
        $inventory.goos -ne 'windows' -or $inventory.goarch -ne $ExpectedArch) { throw 'Notice inventory target mismatch' }
    if ($inventory.binary_sha256 -ne (Get-FileHash -Algorithm SHA256 -LiteralPath $Binary).Hash) {
        throw 'Notice inventory binary checksum mismatch'
    }
    if (@($inventory.components | Where-Object { $_.path -eq 'go' }).Count -ne 1) { throw 'Notice inventory lacks Go runtime' }
    foreach ($component in $inventory.components) {
        if (@($component.notices).Count -eq 0) { throw 'Component has no notice texts' }
        foreach ($notice in $component.notices) {
            if ($notice.file -notmatch '^components/' -or $notice.file -match '(^|/)\.\.(/|$)|\\' -or
                $notice.sha256 -notmatch '^[a-f0-9]{64}$') { throw 'Invalid notice file inventory' }
            $file = Join-Path $Notices $notice.file
            if (-not (Test-Path -LiteralPath $file -PathType Leaf) -or (Get-Item -LiteralPath $file).Length -eq 0) {
                throw "Missing notice file: $($notice.file)"
            }
            if ((Get-FileHash -Algorithm SHA256 -LiteralPath $file).Hash -ne $notice.sha256) {
                throw "Notice file checksum mismatch: $($notice.file)"
            }
        }
    }
    return $inventory
}

$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
$arch = switch ($architecture) {
    'X64' { 'amd64' }
    'Arm64' { 'arm64' }
    default { throw "Unsupported Windows architecture: $architecture" }
}
$asset = "tutor-mcp_windows_$arch.zip"
$base = 'https://github.com/ArnaudGuiovanna/tutor-mcp/releases'
if ($Version -eq 'latest') {
    $base = "$base/latest/download"
} else {
    if ($Version -notmatch '^[a-zA-Z0-9._-]+$') { throw 'Invalid release version' }
    $base = "$base/download/$Version"
}

$temp = Join-Path ([IO.Path]::GetTempPath()) ('tutor-install-' + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $temp | Out-Null
try {
    $archive = Join-Path $temp $asset
    $sums = Join-Path $temp 'SHA256SUMS'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums
    Assert-ArchiveChecksum $archive $sums $asset
    $unpacked = Join-Path $temp 'unpacked'
    Expand-Archive -LiteralPath $archive -DestinationPath $unpacked
    $binary = Join-Path $unpacked 'tutor-mcp.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'Archive does not contain tutor-mcp.exe' }
    $notices = Join-Path $unpacked 'THIRD_PARTY_NOTICES'
    $noticeSource = $unpacked
    $legacySupplement = -not (Test-Path -LiteralPath $notices)
    if ($legacySupplement) {
        if ($Version -notin @('latest', 'v0.6.1')) { throw 'This release predates bundled notices; build from source or use a release with notices' }
        $supplementName = "tutor-mcp_windows_$($arch)_notices.zip"
        $supplement = Join-Path $temp $supplementName
        $supplementSums = Join-Path $temp 'THIRD_PARTY_NOTICES_SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$supplementName" -OutFile $supplement
        Invoke-WebRequest -UseBasicParsing -Uri "$base/THIRD_PARTY_NOTICES_SHA256SUMS" -OutFile $supplementSums
        Assert-ArchiveChecksum $supplement $supplementSums $supplementName
        $noticeSource = Join-Path $temp 'supplement'
        Expand-Archive -LiteralPath $supplement -DestinationPath $noticeSource
        $notices = Join-Path $noticeSource 'THIRD_PARTY_NOTICES'
    }
    $license = Join-Path $noticeSource 'LICENSE'
    foreach ($required in @($license, (Join-Path $notices 'inventory.json'), (Join-Path $notices 'README.txt'))) {
        if (-not (Test-Path -LiteralPath $required -PathType Leaf) -or (Get-Item -LiteralPath $required).Length -eq 0) {
            throw 'Archive does not contain licence texts and third-party notices'
        }
    }
    $inventory = Assert-NoticeInventory $notices $binary $arch
    if ($legacySupplement -and ($inventory.release_version -ne 'v0.6.1' -or
        $inventory.source_commit -ne '440f837e7f043f2f64db23be9459512375dfa542' -or
        $inventory.original_archive_sha256 -ne (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash)) {
        throw 'Legacy notice supplement release or archive checksum mismatch'
    }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $noticeDestination = Join-Path $InstallDir 'tutor-mcp-notices'
    New-Item -ItemType Directory -Force -Path $noticeDestination | Out-Null
    Copy-Item -LiteralPath $notices -Destination $noticeDestination -Recurse -Force
    Copy-Item -LiteralPath $license -Destination (Join-Path $noticeDestination 'LICENSE') -Force
    Copy-Item -LiteralPath $binary -Destination (Join-Path $InstallDir 'tutor-mcp.exe') -Force
    if (-not $NoPathUpdate) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $parts = @($userPath -split ';' | Where-Object { $_ })
        if ($parts -notcontains $InstallDir) {
            [Environment]::SetEnvironmentVariable('Path', (($parts + $InstallDir) -join ';'), 'User')
        }
        if (($env:Path -split ';') -notcontains $InstallDir) { $env:Path += ";$InstallDir" }
    }
    Write-Host "Installed tutor-mcp to $InstallDir. Restart your MCP client to reload PATH."
    Write-Host "Licence texts and third-party notices: $noticeDestination"
    Write-Host 'Local configuration: command: tutor-mcp, args: ["--local"].'
} finally {
    Remove-Item -LiteralPath $temp -Recurse -Force
}
