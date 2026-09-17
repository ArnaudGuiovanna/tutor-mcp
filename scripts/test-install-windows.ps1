# Native Windows acceptance using a real freshly built binary and offline downloads.
$ErrorActionPreference = 'Stop'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('tutor-installer-test-' + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $testRoot | Out-Null
try {
    $binary = Join-Path $testRoot 'tutor-mcp.exe'
    go build -o $binary .
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    $arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() -eq 'Arm64') { 'arm64' } else { 'amd64' }
    $asset = "tutor-mcp_windows_$arch.zip"
    Compress-Archive -LiteralPath $binary -DestinationPath (Join-Path $testRoot $asset)
    $sum = (Get-FileHash -Algorithm SHA256 (Join-Path $testRoot $asset)).Hash
    "$sum  $asset" | Set-Content (Join-Path $testRoot 'SHA256SUMS') -Encoding ascii
    function Invoke-WebRequest {
        param([switch]$UseBasicParsing, [string]$Uri, [string]$OutFile)
        Copy-Item -LiteralPath (Join-Path $testRoot ($Uri.Split('/')[-1])) -Destination $OutFile
    }
    $destination = Join-Path $testRoot 'bin with spaces'
    & "$PSScriptRoot/install.ps1" -Version v0.6.0 -InstallDir $destination -NoPathUpdate
    & (Join-Path $destination 'tutor-mcp.exe') --version
    if ($LASTEXITCODE -ne 0) { throw 'Installed binary failed' }
    $before = (Get-FileHash (Join-Path $destination 'tutor-mcp.exe')).Hash
    "$('0' * 64)  $asset" | Set-Content (Join-Path $testRoot 'SHA256SUMS') -Encoding ascii
    $rejected = $false
    try { & "$PSScriptRoot/install.ps1" -Version v0.6.0 -InstallDir $destination -NoPathUpdate }
    catch { if ($_.Exception.Message -notmatch 'Checksum mismatch') { throw }; $rejected = $true }
    if (-not $rejected) { throw 'Invalid checksum accepted' }
    if ((Get-FileHash (Join-Path $destination 'tutor-mcp.exe')).Hash -ne $before) { throw 'Failed update changed installed binary' }
} finally {
    Remove-Item -LiteralPath $testRoot -Recurse -Force
}
