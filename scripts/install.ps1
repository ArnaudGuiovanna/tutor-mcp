# Install TUTOR MCP for the current Windows user; no administrator rights needed.
[CmdletBinding()]
param(
    [string]$Version = $(if ($env:TUTOR_MCP_VERSION) { $env:TUTOR_MCP_VERSION } else { 'latest' }),
    [string]$InstallDir = $(if ($env:TUTOR_MCP_INSTALL_DIR) { $env:TUTOR_MCP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tutor-mcp' }),
    [switch]$NoPathUpdate
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

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
    $entries = @(Get-Content $sums | Where-Object { $_ -match ('^[a-fA-F0-9]{64}\s+\*?' + [regex]::Escape($asset) + '$') })
    if ($entries.Count -ne 1) { throw "Missing or ambiguous checksum for $asset" }
    $expected = ($entries[0] -split '\s+')[0]
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash
    if ($actual -ne $expected) { throw "Checksum mismatch for $asset" }
    $unpacked = Join-Path $temp 'unpacked'
    Expand-Archive -LiteralPath $archive -DestinationPath $unpacked
    $binary = Join-Path $unpacked 'tutor-mcp.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'Archive does not contain tutor-mcp.exe' }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
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
    Write-Host 'Local configuration: command: tutor-mcp, args: ["--local"].'
} finally {
    Remove-Item -LiteralPath $temp -Recurse -Force
}
