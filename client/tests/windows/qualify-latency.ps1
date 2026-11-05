# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Adapter,
    [Parameter(Mandatory = $true)][string]$TutorBinary,
    [Parameter(Mandatory = $true)][string]$TestDirectory,
    [ValidateSet('native', 'wsl')][string]$Environment = 'native',
    [string]$Distribution,
    [string]$WslUser,
    [string]$AgentDirectory,
    [ValidateSet('codex', 'claude')][string]$Harness = 'codex',
    [ValidateSet('context-read', 'explanation', 'resume', 'correction')][string]$Scenario = 'resume',
    [string]$Model,
    [string]$Effort,
    [ValidateSet('balanced', 'fast', 'deep')][string]$Speed = 'balanced',
    [ValidateRange(2, 30)][int]$Turns = 3,
    [switch]$LiveModel
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $LiveModel) { throw 'Pass -LiveModel to use the selected harness account for actual model calls.' }
if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) { throw 'Run this Windows qualification from PowerShell on Windows.' }
$clientRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$arguments = @('run', '--manifest-path', (Join-Path $clientRoot 'src-tauri/Cargo.toml'), '--locked', '--no-default-features', '--features', 'qualification', '--bin', 'tutor-client-latency', '--', '--live-model', '--adapter', $Adapter, '--tutor', $TutorBinary, '--directory', $TestDirectory, '--environment', $Environment, '--turns', "$Turns")
$arguments += @('--harness', $Harness, '--scenario', $Scenario, '--speed', $Speed)
if ($Model) { $arguments += @('--model', $Model) }
if ($Effort) { $arguments += @('--effort', $Effort) }
if ($Distribution) { $arguments += @('--distribution', $Distribution) }
if ($WslUser) { $arguments += @('--wsl-user', $WslUser) }
if ($AgentDirectory) { $arguments += @('--agent-directory', $AgentDirectory) }
& cargo @arguments
if ($LASTEXITCODE -ne 0) { throw 'Actual model/MCP latency qualification incomplete. Inspect the sanitized latency.json report.' }
