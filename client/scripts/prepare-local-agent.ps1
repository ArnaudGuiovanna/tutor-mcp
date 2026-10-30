# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
# Build the exact CPU sidecar distributed inside the Tutor Windows installer.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
Push-Location $repo
$oldFlags = $env:CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_RUSTFLAGS
$oldCrt = $env:LLAMA_STATIC_CRT
try {
    $commit = (& git rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0 -or $commit -notmatch '^[0-9a-f]{40}$') { throw 'Cannot identify source commit.' }
    & git diff --quiet HEAD -- client/local-agent client/scripts/prepare-local-agent.ps1
    if ($LASTEXITCODE -ne 0) { throw 'Commit local agent sources before packaging.' }
    $cmake = (Get-Command cmake.exe -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
    & $cmake --version
    if ($LASTEXITCODE -ne 0) { throw 'CMake is unavailable.' }
    $env:CMAKE = $cmake
    $clang = Get-Command clang.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    $candidates = @()
    if ($clang) { $candidates += Split-Path -Parent $clang.Source }
    $candidates += Join-Path $env:ProgramFiles 'LLVM/bin'
    $llvm = $candidates | Where-Object { Test-Path -LiteralPath (Join-Path $_ 'libclang.dll') -PathType Leaf } | Select-Object -First 1
    if (-not $llvm) { throw 'LLVM libclang.dll is required for the native inference build.' }
    $env:LIBCLANG_PATH = $llvm
    # No installed VC++ Redistributable or GPU runtime is needed by this CPU EXE.
    # Use an explicit target so host proc-macros are not built with static CRT.
    $env:LLAMA_STATIC_CRT = '1'
    $env:CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_RUSTFLAGS = '-C target-feature=+crt-static'
    & cargo +1.95.0 build --manifest-path client/local-agent/Cargo.toml --locked --release --target x86_64-pc-windows-msvc
    if ($LASTEXITCODE -ne 0) { throw 'Native local agent build failed.' }
    $binary = Join-Path $repo 'client/local-agent/target/x86_64-pc-windows-msvc/release/tutor-local-agent.exe'
    & $binary --version
    if ($LASTEXITCODE -ne 0) { throw 'Local agent version smoke failed.' }
    & $binary --help
    if ($LASTEXITCODE -ne 0) { throw 'Local agent CLI smoke failed.' }
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
    $vs = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
    if ($LASTEXITCODE -ne 0 -or -not $vs) { throw 'Visual Studio C++ tools are required.' }
    $dumpbin = Get-ChildItem (Join-Path $vs 'VC/Tools/MSVC/*/bin/Hostx64/x64/dumpbin.exe') | Sort-Object FullName -Descending | Select-Object -First 1
    if (-not $dumpbin) { throw 'dumpbin.exe is required to verify runtime dependencies.' }
    $dependencies = & $dumpbin.FullName /DEPENDENTS $binary
    if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect native runtime dependencies.' }
    $dlls = @($dependencies | ForEach-Object { if ($_ -match '^\s+([\w.-]+\.dll)\s*$') { $Matches[1].ToLowerInvariant() } } | Sort-Object -Unique)
    if ($dlls.Count -eq 0 -or ($dlls | Where-Object { $_ -match 'vcruntime|msvcp|libomp|cudart|cublas|ggml|llama' })) {
        throw 'CPU sidecar unexpectedly requires an external C++ or inference runtime.'
    }
    $destination = Join-Path $repo 'client/bundled-local-agent'
    [void](New-Item -ItemType Directory -Path $destination -Force)
    Copy-Item -LiteralPath $binary -Destination (Join-Path $destination 'tutor-local-agent.exe')
    Copy-Item -LiteralPath 'client/local-agent/model-profile.json' -Destination $destination
    $modelLicense = 'client/local-agent/notices/QWEN-LICENSE.txt'
    $modelLicenseHash = (Get-FileHash -LiteralPath $modelLicense -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($modelLicenseHash -ne '832dd9e00a68dd83b3c3fb9f5588dad7dcf337a0db50f7d9483f310cd292e92e') {
        throw 'The original pinned Qwen license has changed.'
    }
    Copy-Item -LiteralPath $modelLicense -Destination $destination
    Copy-Item -LiteralPath 'client/local-agent/notices/QWEN-SOURCE.txt' -Destination $destination
    @{
        sourceCommit = $commit; target = 'x86_64-pc-windows-msvc'; backend = 'llama.cpp CPU'
        sha256 = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant()
        bytes = (Get-Item -LiteralPath $binary).Length
        staticCrt = $true; importedDlls = $dlls
        modelLicenseSha256 = $modelLicenseHash
        realModelInference = 'not_tested'; p2000Qualification = 'not_tested'; installer = $true
    } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $destination 'local-agent-build.json') -Encoding utf8NoBOM
    & python client/scripts/collect-local-agent-notices.py collect --target x86_64-pc-windows-msvc --output (Join-Path $destination 'THIRD_PARTY_NOTICES')
    if ($LASTEXITCODE -ne 0) { throw 'Local agent distribution notices are incomplete.' }
} finally {
    $env:CARGO_TARGET_X86_64_PC_WINDOWS_MSVC_RUSTFLAGS = $oldFlags
    $env:LLAMA_STATIC_CRT = $oldCrt
    Pop-Location
}
