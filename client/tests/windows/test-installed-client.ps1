# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param([Parameter(Mandatory = $true)][string]$Installer)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'Ce test doit être exécuté sous Windows.'
}
$installerPath = (Resolve-Path -LiteralPath $Installer).Path
$installRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('Tutor install qualification ' + [guid]::NewGuid().ToString('N'))
$application = $null
try {
    # NSIS requires /D to be last and accepts the remaining path with spaces.
    # Start-Process starts the executable directly, never a shell interpreter.
    $installation = Start-Process -FilePath $installerPath -ArgumentList "/S /D=$installRoot" -PassThru -Wait
    if ($installation.ExitCode -ne 0) { throw "Installation NSIS refusée : $($installation.ExitCode)." }
    $binary = Join-Path $installRoot 'tutor-client.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw "Le binaire du client est absent de l’installation." }
    foreach ($helper in @('tutor-client-qualify.exe', 'tutor-client-smoke-learning.exe')) {
        if (Test-Path -LiteralPath (Join-Path $installRoot $helper)) { throw "Un outil de qualification a été incorporé à l’installation : $helper" }
    }
    $noticeVerifier = (Resolve-Path (Join-Path $PSScriptRoot '../../scripts/collect-notices.py')).Path
    & python $noticeVerifier verify-tree (Join-Path $installRoot 'THIRD_PARTY_NOTICES')
    if ($LASTEXITCODE -ne 0) { throw 'Les notices installées sont absentes, incomplètes ou altérées.' }
    $application = Start-Process -FilePath $binary -PassThru
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $window = [IntPtr]::Zero
    while ([DateTime]::UtcNow -lt $deadline) {
        $application.Refresh()
        if ($application.HasExited) { throw "Le client s’est arrêté au démarrage : $($application.ExitCode)." }
        $window = $application.MainWindowHandle
        if ($window -ne [IntPtr]::Zero) { break }
        Start-Sleep -Milliseconds 250
    }
    if ($window -eq [IntPtr]::Zero) { throw "Aucune fenêtre native du client n’a été créée." }
    Start-Sleep -Seconds 2
    $application.Refresh()
    if ($application.HasExited) { throw "Le client s’est arrêté après la création de la fenêtre." }
    Write-Host "Installation et démarrage Windows réussis : $($application.MainWindowTitle)"
} finally {
    if ($null -ne $application) {
        $application.Refresh()
        if (-not $application.HasExited) {
            [void]$application.CloseMainWindow()
            if (-not $application.WaitForExit(5000)) { Stop-Process -Id $application.Id -Force }
        }
    }
    if (Test-Path -LiteralPath $installRoot -PathType Container) {
        $uninstallers = @(Get-ChildItem -LiteralPath $installRoot -Filter '*uninstall*.exe' -File)
        if ($uninstallers.Count -eq 1) {
            $uninstallation = Start-Process -FilePath $uninstallers[0].FullName -ArgumentList '/S' -PassThru -Wait
            if ($uninstallation.ExitCode -ne 0) { Write-Warning "Désinstallation refusée : $($uninstallation.ExitCode)." }
        }
    }
}
