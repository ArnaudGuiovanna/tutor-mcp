# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Report
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT -or $env:GITHUB_ACTIONS -ne 'true') {
    throw 'This disposable distribution fixture runs only on a Windows GitHub Actions runner. Use qualify-runtime.ps1 for an existing WSL installation.'
}

# Official Ubuntu and Node payloads, pinned to the SHA256SUMS fetched from the
# respective publishers. The manifests are not accepted as new trust roots at
# runtime: changing a payload requires reviewing its pinned digest here too.
$fixtureRootfsUri = 'https://cloud-images.ubuntu.com/wsl/releases/24.04/current/ubuntu-noble-wsl-amd64-24.04lts.rootfs.tar.gz'
$fixtureRootfsDigest = '2a790896740b14d637dbdc583cce1ba081ac53b9e9cdb46dc09a2f73abbd9934'
$fixtureNodeUri = 'https://nodejs.org/dist/v24.13.0/node-v24.13.0-linux-x64.tar.xz'
$fixtureNodeDigest = 'e798599612f4bb71333a3397ab0d095fd62214e115aea45aa858a145fc72d67e'
$fixtureName = 'TutorClientCI-' + [guid]::NewGuid().ToString('N')
$fixtureRoot = Join-Path $env:RUNNER_TEMP $fixtureName
$fixtureInstall = Join-Path $fixtureRoot 'distribution'
$fixtureRootfs = Join-Path $fixtureRoot 'ubuntu-rootfs.tar.gz'
$fixtureNode = Join-Path $fixtureRoot 'node-linux-x64.tar.xz'
$fixtureRepo = (Resolve-Path (Join-Path $PSScriptRoot '../../..')).Path
$fixtureImportAttempted = $false

function Get-VerifiedFixturePayload([string]$Uri, [string]$Destination, [string]$Digest) {
    $ProgressPreference = 'SilentlyContinue'
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        try {
            Invoke-WebRequest -Uri $Uri -OutFile $Destination -TimeoutSec 240
            break
        } catch {
            if ($attempt -eq 3) { throw }
            Start-Sleep -Seconds 2
        }
    }
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $Destination).Hash.ToLowerInvariant()
    if ($actual -ne $Digest) { throw "Official fixture payload hash mismatch: $Uri" }
}

New-Item -ItemType Directory -Path $fixtureRoot -ErrorAction Stop | Out-Null
try {
    & wsl.exe --version
    if ($LASTEXITCODE -ne 0) { throw 'The Windows runner does not provide the current WSL runtime.' }
    Get-VerifiedFixturePayload $fixtureRootfsUri $fixtureRootfs $fixtureRootfsDigest
    Get-VerifiedFixturePayload $fixtureNodeUri $fixtureNode $fixtureNodeDigest

    # Explicit import version: a successful WSL1 installation cannot make this
    # acceptance check green. No existing distribution or default is replaced.
    $fixtureImportAttempted = $true
    & wsl.exe --import $fixtureName $fixtureInstall $fixtureRootfs --version 2
    if ($LASTEXITCODE -ne 0) { throw 'The runner could not import/start WSL2. Inspect virtualization and WSL diagnostics; WSL1 is not an accepted fallback.' }
    $list = ((& wsl.exe --list --verbose) | Out-String) -replace "`0", ''
    if ($LASTEXITCODE -ne 0 -or $list -notmatch ('(?m)^\s*\*?\s*' + [regex]::Escape($fixtureName) + '\s+.+\s+2\s*$')) {
        throw 'The imported fixture is not reported as a WSL2 distribution.'
    }
    Write-Host $list

    $linuxRepo = ((& wsl.exe --distribution $fixtureName --user root --exec wslpath -u $fixtureRepo) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $linuxRepo.StartsWith('/')) { throw 'Cannot resolve the checkout into the Linux fixture.' }
    $linuxNode = ((& wsl.exe --distribution $fixtureName --user root --exec wslpath -u $fixtureNode) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $linuxNode.StartsWith('/')) { throw 'Cannot resolve the verified Node archive into the Linux fixture.' }
    & wsl.exe --distribution $fixtureName --user root --exec /bin/bash --noprofile --norc "$linuxRepo/client/tests/windows/setup-wsl-runtime.sh" "$linuxRepo/client/scripts/adapter" $linuxNode
    if ($LASTEXITCODE -ne 0) { throw 'The locked Linux adapter fixture could not be prepared.' }

    # Apply this fixture's default nonroot user, leaving other distributions and
    # their processes alone. Warm it before bounded desktop discovery probes.
    & wsl.exe --terminate $fixtureName
    if ($LASTEXITCODE -ne 0) { throw 'Cannot restart the disposable fixture with its regular Linux user.' }
    $linuxUser = ((& wsl.exe --distribution $fixtureName --exec id -un) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $linuxUser -ne 'tutorci') { throw 'The fixture did not select its regular Linux user.' }
    $linuxUid = ((& wsl.exe --distribution $fixtureName --exec id -u) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $linuxUid -eq '0') { throw 'WSL discovery must be qualified as a nonroot Linux user.' }
    $linuxCodexVersion = ((& wsl.exe --distribution $fixtureName --exec /usr/bin/env 'PATH=/home/tutorci/.local/bin:/usr/bin:/bin' codex --version) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $linuxCodexVersion -notmatch '^codex-cli\s+\S+') { throw 'The fixture Codex CLI did not run under its Linux user.' }

    # No adapter override: this exercises actual Windows enumeration, UTF16
    # parsing, default-user discovery, PATH lookup and the WSL stdio bridge.
    & (Join-Path $PSScriptRoot 'qualify-runtime.ps1') -Environment wsl -Distribution $fixtureName -AgentDirectory '/home/tutorci/mes cours' -Report $Report
    $qualification = Get-Content -LiteralPath $Report -Raw | ConvertFrom-Json
    if ($qualification.host -ne 'windows' -or $qualification.environment -ne 'wsl' -or $qualification.distribution -ne $fixtureName -or $qualification.wslUser -ne 'tutorci' -or $qualification.launchInitializeStop -ne 'passed' -or $qualification.agent.version -ne '2.2.2' -or $qualification.codexPath -ne '/home/tutorci/.local/bin/codex' -or $qualification.adapterPath -ne '/home/tutorci/.local/bin/codex-acp') {
        throw 'The runtime report does not prove discovery and ACP initialization of the installed WSL agent.'
    }
    if ($qualification.codexRuntime -ne 'adapter-bundled') { throw 'Automatic discovery unexpectedly overrode the adapter runtime.' }
    & wsl.exe --distribution $fixtureName --exec /usr/bin/test ! -e /home/tutorci/.codex/tutor-ci-selected-cli
    if ($LASTEXITCODE -ne 0) { throw 'The automatically detected CLI was used as an implicit override.' }

    $manualReport = Join-Path $fixtureRoot 'manual-cli-runtime.json'
    & (Join-Path $PSScriptRoot 'qualify-runtime.ps1') -Environment wsl -Distribution $fixtureName -AgentDirectory '/home/tutorci/mes cours' -CodexBinary '/home/tutorci/.local/bin/codex' -Report $manualReport
    $manualQualification = Get-Content -LiteralPath $manualReport -Raw | ConvertFrom-Json
    if ($manualQualification.codexRuntime -ne 'explicit-cli' -or $manualQualification.launchInitializeStop -ne 'passed' -or $manualQualification.wslUser -ne 'tutorci') { throw 'The explicit installed CLI did not negotiate the WSL ACP connection.' }
    & wsl.exe --distribution $fixtureName --exec /usr/bin/test -f /home/tutorci/.codex/tutor-ci-selected-cli
    if ($LASTEXITCODE -ne 0) { throw 'The adapter did not start the explicitly selected installed Codex CLI.' }

    $clean = $false
    for ($attempt = 0; $attempt -lt 20; $attempt++) {
        & wsl.exe --distribution $fixtureName --user root --exec /bin/bash --noprofile --norc -c 'if pgrep -u tutorci -f "codex-acp|codex app-server"; then exit 42; else code=$?; [ "$code" -eq 1 ] || exit "$code"; fi'
        if ($LASTEXITCODE -eq 0) { $clean = $true; break }
        if ($LASTEXITCODE -ne 42) { throw 'Could not inspect the fixture Linux process lifecycle.' }
        Start-Sleep -Milliseconds 250
    }
    if (-not $clean) { throw 'Codex or adapter processes survived the client stop inside WSL.' }
    $qualification | Add-Member -NotePropertyName fixture -NotePropertyValue @{
        wslDistributionVersion = 2
        linuxUser = $linuxUser
        linuxUid = $linuxUid
        codexVersion = $linuxCodexVersion
        processCleanup = 'passed'
        explicitInstalledCli = 'passed'
        ubuntuRootfsUrl = $fixtureRootfsUri
        ubuntuRootfsSHA256 = $fixtureRootfsDigest
        nodeArchiveUrl = $fixtureNodeUri
        nodeArchiveSHA256 = $fixtureNodeDigest
        adapterLockSHA256 = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $fixtureRepo 'client/scripts/adapter/package-lock.json')).Hash.ToLowerInvariant()
    }
    $qualification | ConvertTo-Json -Depth 30 | Set-Content -LiteralPath $Report -Encoding utf8
    Write-Host 'Windows → WSL2 → actual Codex ACP discovery, initialize and process cleanup passed.'
} finally {
    if ($fixtureImportAttempted) {
        # Import failures can leave a partially registered fixture. Enumerate
        # and remove only this generated name, including in that failure case.
        $registered = @((& wsl.exe --list --quiet) | ForEach-Object { ($_ -replace "`0", '').Trim() })
        if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect disposable distribution cleanup.' }
        if ($registered -contains $fixtureName) {
            & wsl.exe --unregister $fixtureName
            if ($LASTEXITCODE -ne 0) { throw "Cannot remove the disposable CI distribution $fixtureName." }
        }
    }
    # The directory name is generated here under RUNNER_TEMP; no caller path or
    # pre-existing Windows/WSL user data is ever removed by this fixture.
    Remove-Item -LiteralPath $fixtureRoot -Recurse -Force -ErrorAction SilentlyContinue
}
