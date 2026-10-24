# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param(
    [ValidateSet('native', 'wsl')][string]$Environment = 'native',
    [string]$Distribution,
    [string]$Adapter,
    [string]$CodexBinary,
    [string]$AgentDirectory,
    [string]$TutorBinary,
    [string]$Report
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'Exécuter cette qualification depuis Windows avec PowerShell.'
}
if ($Environment -eq 'wsl' -and [string]::IsNullOrWhiteSpace($Distribution)) {
    throw 'Indiquer explicitement -Distribution, par exemple Ubuntu. Aucune distribution ne sera installée ou modifiée.'
}
if ($Environment -eq 'native' -and -not [string]::IsNullOrWhiteSpace($Distribution)) {
    throw '-Distribution concerne uniquement WSL.'
}

$clientRoot = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$arguments = @('run', '--manifest-path', (Join-Path $clientRoot 'src-tauri/Cargo.toml'),
    '--locked', '--no-default-features', '--features', 'qualification', '--bin', 'tutor-client-qualify', '--', '--environment', $Environment)
if ($Distribution) { $arguments += @('--distribution', $Distribution) }
if ($Adapter) { $arguments += @('--adapter', $Adapter) }
if ($CodexBinary) { $arguments += @('--codex', $CodexBinary) }
if ($AgentDirectory) { $arguments += @('--cwd', $AgentDirectory) }
if ($TutorBinary) { $arguments += @('--tutor', $TutorBinary) }
if ($Report) { $arguments += @('--report', $Report) }

& cargo @arguments
if ($LASTEXITCODE -ne 0) { throw "La qualification du runtime a échoué (code $LASTEXITCODE)." }
Write-Host "Qualification du lancement, du protocole et de l’arrêt terminée. Aucun appel de modèle effectué."
