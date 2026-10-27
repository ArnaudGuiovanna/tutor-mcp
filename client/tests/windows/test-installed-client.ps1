# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Installer,
    [string]$Report = ''
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ([System.Environment]::OSVersion.Platform -ne [System.PlatformID]::Win32NT) {
    throw 'Ce test doit être exécuté sous Windows.'
}
if (-not $Report) {
    $reportRoot = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { [System.IO.Path]::GetTempPath() }
    $Report = Join-Path $reportRoot 'tutor-client-accessibility.json'
}
$installerPath = (Resolve-Path -LiteralPath $Installer).Path
$configPath = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '../../src-tauri/tauri.conf.json')).Path
$config = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
$identifier = [string]$config.identifier
if ($identifier -notmatch '^[A-Za-z0-9.-]+$') { throw 'Unexpected application identifier in the qualification config.' }
$appData = Join-Path ([Environment]::GetFolderPath('ApplicationData')) $identifier
$webviewData = Join-Path ([Environment]::GetFolderPath('LocalApplicationData')) $identifier
$testDirectories = @(@($appData, $webviewData) | Select-Object -Unique)
# The real Tauri app_data_dir uses RoamingAppData/identifier. Never replace,
# rename or erase a pre-existing user account to make a test appear isolated.
foreach ($directory in $testDirectories) {
    if (Test-Path -LiteralPath $directory) {
        throw "This qualification requires a fresh Windows account; existing Tutor data is preserved: $directory"
    }
}
$installRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('Tutor install qualification ' + [guid]::NewGuid().ToString('N'))
$application = $null
$ownsTestData = $false
$workspacePath = Join-Path $appData 'workspace.json'

function Start-TestApplication([string]$Binary) {
    $process = Start-Process -FilePath $Binary -PassThru
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $window = [IntPtr]::Zero
    do {
        $process.Refresh()
        if ($process.HasExited) { throw "Le client s’est arrêté au démarrage : $($process.ExitCode)." }
        $window = $process.MainWindowHandle
        if ($window -ne [IntPtr]::Zero) { break }
        Start-Sleep -Milliseconds 250
    } while ([DateTime]::UtcNow -lt $deadline)
    if ($window -eq [IntPtr]::Zero) {
        Stop-Process -Id $process.Id -Force
        throw "Aucune fenêtre native du client n’a été créée."
    }
    Start-Sleep -Seconds 2
    $process.Refresh()
    if ($process.HasExited) { throw "Le client s’est arrêté après la création de la fenêtre." }
    return @{ Process = $process; Window = $window }
}
function Assert-SavedSettings([string]$Language, [string]$Theme) {
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    do {
        $workspace = Get-Content -LiteralPath $workspacePath -Raw | ConvertFrom-Json
        if ($workspace.settings.tutor.mode -ne 'local' -or $workspace.settings.tutor.command -ne '' -or $workspace.settings.tutor.url -ne '') {
            throw 'The qualification profile unexpectedly acquired a network connection.'
        }
        if ($workspace.settings.language -eq $Language -and $workspace.settings.appearance.theme -eq $Theme) { return }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Settings were not persisted as $Language/$Theme in the actual application workspace."
}
try {
    # NSIS requires /D to be last and accepts the remaining path with spaces.
    $installation = Start-Process -FilePath $installerPath -ArgumentList "/S /D=$installRoot" -PassThru -Wait
    if ($installation.ExitCode -ne 0) { throw "Installation NSIS refusée : $($installation.ExitCode)." }
    $binary = Join-Path $installRoot 'tutor-client.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw "Le binaire du client est absent de l’installation." }
    foreach ($helper in @('tutor-client-qualify.exe', 'tutor-client-smoke.exe')) {
        if (Test-Path -LiteralPath (Join-Path $installRoot $helper)) { throw "Un outil de qualification a été incorporé à l’installation : $helper" }
    }
    $noticeVerifier = (Resolve-Path (Join-Path $PSScriptRoot '../../scripts/collect-notices.py')).Path
    & python $noticeVerifier verify-tree (Join-Path $installRoot 'THIRD_PARTY_NOTICES')
    if ($LASTEXITCODE -ne 0) { throw 'Les notices installées sont absentes, incomplètes ou altérées.' }
    $agentVerifier = (Resolve-Path (Join-Path $PSScriptRoot '../../scripts/verify-packaged-agents.py')).Path
    & python $agentVerifier (Join-Path $installRoot 'agents')
    if ($LASTEXITCODE -ne 0) { throw 'La qualification des agents installés a échoué ; voir le détail ci-dessus.' }

    [void](New-Item -ItemType Directory -Path $appData)
    $ownsTestData = $true
    $fixture = @{
        schemaVersion = 1
        settings = @{
            targetId = ''; cwd = ''; manualTarget = $null
            tutor = @{ mode = 'local'; activity = 'auto'; url = ''; command = ''; args = @() }
            language = 'fr'
            appearance = @{ theme = 'dark'; fontSize = 20; lineHeight = 1.65; reduceMotion = $false }
            agent = @{ model = ''; reasoningEffort = ''; speed = 'balanced' }
            permissionPolicy = $null
        }
        intents = @(); conversations = @(); activeConversationId = $null
    }
    [IO.File]::WriteAllText($workspacePath, ($fixture | ConvertTo-Json -Depth 10), ([System.Text.UTF8Encoding]::new($false)))
    $accessibility = Join-Path $PSScriptRoot 'test-accessibility.ps1'
    $exerciseReport = "$Report.exercise.json"
    $restartReport = "$Report.restart.json"

    $launch = Start-TestApplication $binary
    $application = $launch.Process
    & powershell.exe -NoProfile -STA -File $accessibility -NativeHandle ($launch.Window.ToInt64()) -Report $exerciseReport -TestWorkspace $workspacePath -Mode Exercise
    if ($LASTEXITCODE -ne 0) { throw 'La navigation accessible du client installé a échoué.' }
    Assert-SavedSettings 'en' 'light'
    if (-not $application.WaitForExit(10000)) {
        throw 'The application did not close cleanly before persistence qualification.'
    }
    $application.Dispose()
    $application = $null

    # Same executable, same profile, a new OS process and no data reseeding.
    $launch = Start-TestApplication $binary
    $application = $launch.Process
    Assert-SavedSettings 'en' 'light'
    & powershell.exe -NoProfile -STA -File $accessibility -NativeHandle ($launch.Window.ToInt64()) -Report $restartReport -TestWorkspace $workspacePath -Mode VerifyPersistence
    if ($LASTEXITCODE -ne 0) { throw 'La persistance et la navigation après réouverture ont échoué.' }
    Assert-SavedSettings 'fr' 'dark'
    if (-not $application.WaitForExit(10000)) { throw 'The custom close button did not close the reopened application.' }
    $exercise = Get-Content -LiteralPath $exerciseReport -Raw | ConvertFrom-Json
    $reopened = Get-Content -LiteralPath $restartReport -Raw | ConvertFrom-Json
    if (-not $exercise.languageSwitchFrEnFr -or -not $exercise.commandPaletteKeyboardNavigation -or -not $reopened.persistenceAfterProcessRestart) {
        throw 'Incomplete language or process-restart evidence.'
    }
    foreach ($check in @('nativeCaptionAbsent','customMinimize','customMaximizeRestore','titlebarDrag','titlebarDoubleClick','edgeResize')) {
        if (-not $exercise.windowChrome.$check) { throw "Missing actual native window evidence: $check" }
    }
    $reportParent = Split-Path -Parent $Report
    if ($reportParent -and -not (Test-Path -LiteralPath $reportParent)) { [void](New-Item -ItemType Directory -Path $reportParent) }
    @{
        platform = 'Windows'; sourceCommit = $env:GITHUB_SHA
        sourceVersion = [string]$config.version
        binaryProductVersion = [System.Diagnostics.FileVersionInfo]::GetVersionInfo($binary).ProductVersion
        uiaNames = $true; keyboardInput = $true; keyboardNavigation = $true
        settingsTabs = @('agents', 'institution', 'members', 'ergonomics')
        languageSwitchFrEnFr = $true; themeSwitchLightDark = $true
        commandPaletteKeyboardNavigation = $true
        framelessWindow = $true; customClose = $true
        persistenceAfterProcessRestart = $true; persistedValuesVerifiedInWorkspace = $true
        networkRequestSent = $false; testOwnedOfflineProfile = $true
        sendBeforeSetup = $true; narratorManualAudit = $false
        exercise = $exercise; reopened = $reopened
    } | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $Report -Encoding UTF8
    if (-not (Test-Path -LiteralPath $Report -PathType Leaf)) { throw 'The UI Automation report was not written.' }
    Write-Host "Windows installation, accessible settings and persistence passed. Report: $Report"
} finally {
    if ($null -ne $application) {
        $application.Refresh()
        if (-not $application.HasExited) {
            [void]$application.CloseMainWindow()
            if (-not $application.WaitForExit(5000)) { Stop-Process -Id $application.Id -Force }
        }
    }
    if ($ownsTestData) {
        foreach ($directory in $testDirectories) {
            # WebView2 may release its cache handles shortly after the window
            # process exits. Retry only these test-owned directories.
            $cleanupDeadline = [DateTime]::UtcNow.AddSeconds(10)
            while (Test-Path -LiteralPath $directory) {
                try {
                    Remove-Item -LiteralPath $directory -Recurse -Force
                } catch {
                    if ([DateTime]::UtcNow -ge $cleanupDeadline) { throw }
                    Start-Sleep -Milliseconds 250
                }
            }
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
