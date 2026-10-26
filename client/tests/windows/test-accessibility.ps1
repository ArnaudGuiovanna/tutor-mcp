# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][long]$NativeHandle,
    [Parameter(Mandatory = $true)][string]$Report,
    [Parameter(Mandatory = $true)][string]$TestWorkspace,
    [ValidateSet('Exercise', 'VerifyPersistence')][string]$Mode = 'Exercise'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
# This is a test-owned, unauthenticated offline profile. Refuse to exercise a
# normal account or a remote connection while claiming no MCP request was sent.
$fixture = Get-Content -LiteralPath $TestWorkspace -Raw | ConvertFrom-Json
if ($fixture.settings.tutor.mode -ne 'local' -or $fixture.settings.tutor.command -ne '' -or $fixture.settings.tutor.url -ne '' -or @($fixture.conversations).Count -ne 0) {
    throw 'UI Automation requires the fresh offline test workspace.'
}
if (Test-Path -LiteralPath (Join-Path (Split-Path -Parent $TestWorkspace) 'native-identity.json')) {
    throw 'UI Automation must not exercise an existing identity.'
}
Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes, WindowsBase, System.Windows.Forms
Add-Type @'
using System;
using System.Runtime.InteropServices;
public static class TutorFocus {
 [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr window);
}
'@
[void][TutorFocus]::SetForegroundWindow([IntPtr]$NativeHandle)
$root = [System.Windows.Automation.AutomationElement]::FromHandle([IntPtr]$NativeHandle)
$buttonType = [System.Windows.Automation.ControlType]::Button
$tabType = [System.Windows.Automation.ControlType]::TabItem
$comboType = [System.Windows.Automation.ControlType]::ComboBox
$editType = [System.Windows.Automation.ControlType]::Edit

function Find-Control([string]$Name, [System.Windows.Automation.ControlType]$Type, [bool]$Prefix = $false) {
    $condition = New-Object System.Windows.Automation.PropertyCondition(
        [System.Windows.Automation.AutomationElement]::ControlTypeProperty, $Type)
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $elements = $root.FindAll([System.Windows.Automation.TreeScope]::Descendants, $condition)
        foreach ($element in $elements) {
            $currentName = $element.Current.Name
            if ($currentName -eq $Name -or ($Prefix -and $currentName.StartsWith($Name, [StringComparison]::Ordinal))) {
                return $element
            }
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Accessible control missing: $Name ($Type)"
}
function Activate-Control([System.Windows.Automation.AutomationElement]$Element) {
    $pattern = $null
    if ($Element.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$pattern)) {
        ([System.Windows.Automation.SelectionItemPattern]$pattern).Select()
    } elseif ($Element.TryGetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern, [ref]$pattern)) {
        ([System.Windows.Automation.InvokePattern]$pattern).Invoke()
    } else {
        $Element.SetFocus()
        [System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
    }
}
function Assert-TabFocus([string]$Name) {
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $focused = [System.Windows.Automation.AutomationElement]::FocusedElement
        if ($null -ne $focused -and $focused.Current.Name -eq $Name -and $focused.Current.IsKeyboardFocusable) {
            $pattern = $null
            $selected = $true
            if ($focused.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$pattern)) {
                $selected = ([System.Windows.Automation.SelectionItemPattern]$pattern).Current.IsSelected
            }
            if ($selected) { return }
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Keyboard navigation did not select the settings tab: $Name"
}
function Select-ComboByKeyboard([string]$Name, [int]$Index, [bool]$Prefix = $false) {
    $element = Find-Control $Name $comboType $Prefix
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    while (-not $element.Current.IsEnabled) {
        if ([DateTime]::UtcNow -ge $deadline) { throw "Settings control remained disabled: $Name" }
        Start-Sleep -Milliseconds 150
        $element = Find-Control $Name $comboType $Prefix
    }
    if (-not $element.Current.IsKeyboardFocusable) { throw "Settings control cannot receive keyboard focus: $Name" }
    $element.SetFocus()
    $keys = '{HOME}' + ('{DOWN}' * $Index) + '{ENTER}'
    [System.Windows.Forms.SendKeys]::SendWait($keys)
}
function Read-ComboValue([System.Windows.Automation.AutomationElement]$Element) {
    $pattern = $null
    if ($Element.TryGetCurrentPattern([System.Windows.Automation.SelectionPattern]::Pattern, [ref]$pattern)) {
        $selection = @(([System.Windows.Automation.SelectionPattern]$pattern).GetCurrentSelection())
        if ($selection.Length -eq 1) { return $selection[0].Current.Name }
    }
    if ($Element.TryGetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern, [ref]$pattern)) {
        return ([System.Windows.Automation.ValuePattern]$pattern).Current.Value
    }
    return ''
}
function Assert-ComboValue([string]$Name, [string[]]$Expected, [bool]$Prefix = $false) {
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    do {
        $element = Find-Control $Name $comboType $Prefix
        $value = Read-ComboValue $element
        if ($element.Current.IsEnabled -and $Expected -contains $value) { return }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Accessible selected value for $Name was '$value'; expected $($Expected -join ' or ')."
}
function Open-Settings([bool]$English) {
    $label = if ($English) { 'Settings' } else { 'Paramètres' }
    Activate-Control (Find-Control $label $buttonType)
    $names = if ($English) { @('Agents', 'Institution', 'Members', 'Reading and appearance') } else { @('Agents', 'Institution', 'Membres', 'Ergonomie') }
    foreach ($name in $names) { [void](Find-Control $name $tabType) }
    $agents = Find-Control 'Agents' $tabType
    $agents.SetFocus()
    [System.Windows.Forms.SendKeys]::SendWait('{HOME}')
    Assert-TabFocus 'Agents'
    foreach ($name in $names[1..3]) {
        [System.Windows.Forms.SendKeys]::SendWait('{RIGHT}')
        Assert-TabFocus $name
    }
}

if ($Mode -eq 'Exercise') {
    $conversation = Find-Control 'Conversation' $buttonType
    $formations = Find-Control 'Formations' $buttonType
    $composer = Find-Control 'Votre message au tuteur' $editType
    if (-not $composer.Current.IsKeyboardFocusable) { throw 'The composer cannot receive keyboard focus.' }
    $composer.SetFocus()
    [System.Windows.Forms.SendKeys]::SendWait('Je veux apprendre le Go')
    Start-Sleep -Milliseconds 300
    $send = Find-Control 'Envoyer' $buttonType
    if (-not $send.Current.IsEnabled) { throw 'Typing did not enable sending before setup.' }
    # The message is typed but never submitted. The test profile is offline.
    Activate-Control $formations
    [void](Find-Control 'Formations' $buttonType)
    Activate-Control $conversation
    $composer = Find-Control 'Votre message au tuteur' $editType
    $composer.SetFocus()
    [System.Windows.Forms.SendKeys]::SendWait('{TAB}')
    $focused = [System.Windows.Automation.AutomationElement]::FocusedElement
    if ($null -eq $focused -or -not $focused.Current.IsKeyboardFocusable) { throw 'Keyboard navigation lost accessible focus.' }

    Open-Settings $false
    Assert-ComboValue 'Langue de Tutor' @('Français', 'fr')
    Select-ComboByKeyboard 'Langue de Tutor' 1
    [void](Find-Control 'Settings' $buttonType)
    [void](Find-Control 'Reading and appearance' $tabType)
    Assert-ComboValue 'Tutor language' @('English', 'en')
    Select-ComboByKeyboard 'Tutor language' 0
    [void](Find-Control 'Paramètres' $buttonType)
    Assert-ComboValue 'Langue de Tutor' @('Français', 'fr')

    Select-ComboByKeyboard 'Apparence' 1 $true
    Assert-ComboValue 'Apparence' @('Clair', 'light') $true
    Select-ComboByKeyboard 'Apparence' 0 $true
    Assert-ComboValue 'Apparence' @('Sombre', 'dark') $true
    # Leave different persisted values for a second OS-process launch.
    Select-ComboByKeyboard 'Langue de Tutor' 1
    Assert-ComboValue 'Tutor language' @('English', 'en')
    Select-ComboByKeyboard 'Appearance' 1 $true
    Assert-ComboValue 'Appearance' @('Light', 'light') $true
    Activate-Control (Find-Control 'Conversation' $buttonType)
    [void](Find-Control 'Your message to the tutor' $editType)
} else {
    # This process has reopened the same test profile, without re-seeding it.
    [void](Find-Control 'Settings' $buttonType)
    Open-Settings $true
    Assert-ComboValue 'Tutor language' @('English', 'en')
    Assert-ComboValue 'Appearance' @('Light', 'light') $true
    Select-ComboByKeyboard 'Tutor language' 0
    Assert-ComboValue 'Langue de Tutor' @('Français', 'fr')
    Select-ComboByKeyboard 'Apparence' 0 $true
    Assert-ComboValue 'Apparence' @('Sombre', 'dark') $true
    Activate-Control (Find-Control 'Conversation' $buttonType)
    [void](Find-Control 'Votre message au tuteur' $editType)
}
$parent = Split-Path -Parent $Report
if ($parent -and -not (Test-Path -LiteralPath $parent)) { [void](New-Item -ItemType Directory -Path $parent) }
@{
    platform = 'Windows'
    mode = $Mode
    uiaNames = $true
    keyboardInput = $true
    keyboardNavigation = $true
    settingsTabs = @('agents', 'institution', 'members', 'ergonomics')
    settingsTabKeyboardNavigation = $true
    languageSwitchFrEnFr = ($Mode -eq 'Exercise')
    themeSwitchLightDark = $true
    persistenceAfterProcessRestart = ($Mode -eq 'VerifyPersistence')
    networkRequestSent = $false
    offlineFixtureRequired = $true
    sendBeforeSetup = ($Mode -eq 'Exercise')
    narratorManualAudit = $false
} | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $Report -Encoding UTF8
Write-Host "UI Automation names, keyboard settings, language and themes passed ($Mode)."
