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
function Get-FocusDescription {
    try {
        $focused = [System.Windows.Automation.AutomationElement]::FocusedElement
        if ($null -eq $focused) { return 'FocusedElement=<null>' }
        $current = $focused.Current
        return "Name='$($current.Name)'; ControlType=$($current.ControlType.ProgrammaticName); HasKeyboardFocus=$($current.HasKeyboardFocus); IsKeyboardFocusable=$($current.IsKeyboardFocusable); IsEnabled=$($current.IsEnabled); IsOffscreen=$($current.IsOffscreen)"
    } catch [System.Windows.Automation.ElementNotAvailableException] {
        return 'FocusedElement was replaced while reading the accessibility tree.'
    }
}
function Assert-ControlFocus([string]$Name, [System.Windows.Automation.ControlType]$Type, [bool]$Prefix = $false) {
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    $stableSamples = 0
    do {
        try {
            $focused = [System.Windows.Automation.AutomationElement]::FocusedElement
            if ($null -ne $focused) {
                $current = $focused.Current
                $matchesName = $current.Name -eq $Name -or ($Prefix -and $current.Name.StartsWith($Name, [StringComparison]::Ordinal))
                if ($matchesName -and $current.ControlType -eq $Type -and $current.HasKeyboardFocus -and $current.IsKeyboardFocusable -and $current.IsEnabled) {
                    $stableSamples++
                    if ($stableSamples -ge 2) { return }
                } else { $stableSamples = 0 }
            } else { $stableSamples = 0 }
        } catch [System.Windows.Automation.ElementNotAvailableException] {
            $stableSamples = 0
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Keyboard focus did not settle on '$Name' ($($Type.ProgrammaticName)). $(Get-FocusDescription)"
}
function Find-ReadyControl([string]$Name, [System.Windows.Automation.ControlType]$Type, [bool]$Prefix = $false) {
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    do {
        $element = Find-Control $Name $Type $Prefix
        try {
            if ($element.Current.IsEnabled -and $element.Current.IsKeyboardFocusable) { return $element }
        } catch [System.Windows.Automation.ElementNotAvailableException] {
            # A React navigation or settings save may replace this element.
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Accessible control did not become ready: $Name ($($Type.ProgrammaticName)). $(Get-FocusDescription)"
}
function Focus-Control([string]$Name, [System.Windows.Automation.ControlType]$Type, [bool]$Prefix = $false) {
    $element = Find-ReadyControl $Name $Type $Prefix
    $element.SetFocus()
    # SetFocus and WebView2's UI Automation focus event are asynchronous.
    # Confirm the actual descendant before sending any keyboard input.
    Assert-ControlFocus $Name $Type $Prefix
}
function Activate-Control([System.Windows.Automation.AutomationElement]$Element) {
    $pattern = $null
    if ($Element.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$pattern)) {
        ([System.Windows.Automation.SelectionItemPattern]$pattern).Select()
    } elseif ($Element.TryGetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern, [ref]$pattern)) {
        ([System.Windows.Automation.InvokePattern]$pattern).Invoke()
    } else {
        $Element.SetFocus()
        Assert-ControlFocus $Element.Current.Name $Element.Current.ControlType
        [System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
    }
}
function Assert-TabFocus([string]$Name) {
    Assert-ControlFocus $Name $tabType
    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $focused = [System.Windows.Automation.AutomationElement]::FocusedElement
        if ($null -ne $focused -and $focused.Current.Name -eq $Name -and $focused.Current.ControlType -eq $tabType -and $focused.Current.HasKeyboardFocus -and $focused.Current.IsKeyboardFocusable) {
            $pattern = $null
            $selected = $true
            if ($focused.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$pattern)) {
                $selected = ([System.Windows.Automation.SelectionItemPattern]$pattern).Current.IsSelected
            }
            if ($selected) { return }
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Keyboard navigation did not select the settings tab: $Name. $(Get-FocusDescription)"
}
function Select-ComboByKeyboard([string]$Name, [int]$Index, [bool]$Prefix = $false) {
    Focus-Control $Name $comboType $Prefix
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
    Focus-Control 'Agents' $tabType
    [System.Windows.Forms.SendKeys]::SendWait('{HOME}')
    Assert-TabFocus 'Agents'
    foreach ($name in $names[1..3]) {
        [System.Windows.Forms.SendKeys]::SendWait('{RIGHT}')
        Assert-TabFocus $name
    }
}

function Find-CommandControl([string]$Name, [System.Windows.Automation.ControlType[]]$Types) {
    $condition = New-Object System.Windows.Automation.PropertyCondition(
        [System.Windows.Automation.AutomationElement]::NameProperty, $Name)
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    do {
        foreach ($element in $root.FindAll([System.Windows.Automation.TreeScope]::Descendants, $condition)) {
            if ($Types -contains $element.Current.ControlType -and $element.Current.IsEnabled -and -not $element.Current.IsOffscreen) { return $element }
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Command palette control missing: $Name. $(Get-FocusDescription)"
}

function Assert-CommandPaletteFocus([System.Windows.Automation.AutomationElement]$Search) {
    $commands = Find-Control 'Commandes' ([System.Windows.Automation.ControlType]::List)
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    $stableSamples = 0
    do {
        try {
            $focused = [System.Windows.Automation.AutomationElement]::FocusedElement
            $ownedFocus = $false
            if ($null -ne $focused -and $focused.Current.HasKeyboardFocus -and $focused.Current.IsEnabled) {
                $ownedFocus = [System.Windows.Automation.Automation]::Compare($focused, $Search)
                # UIA may report the ARIA active descendant while DOM focus stays
                # in the editable combobox. Accept only an option in its list.
                if (-not $ownedFocus -and @($buttonType, [System.Windows.Automation.ControlType]::ListItem) -contains $focused.Current.ControlType) {
                    $ancestor = $focused
                    while ($null -ne $ancestor -and -not $ownedFocus) {
                        $ownedFocus = [System.Windows.Automation.Automation]::Compare($ancestor, $commands)
                        $ancestor = [System.Windows.Automation.TreeWalker]::RawViewWalker.GetParent($ancestor)
                    }
                }
            }
            if ($ownedFocus) { $stableSamples++ } else { $stableSamples = 0 }
            if ($stableSamples -ge 2) { return }
        } catch [System.Windows.Automation.ElementNotAvailableException] {
            $stableSamples = 0
        }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Keyboard focus did not settle in the command palette. $(Get-FocusDescription)"
}

function Exercise-CommandPalette {
    [System.Windows.Forms.SendKeys]::SendWait('^k')
    # Chromium can expose an editable ARIA combobox as ComboBox or Edit.
    $search = Find-CommandControl 'Rechercher une commande' @($comboType, $editType)
    $search.SetFocus()
    Assert-CommandPaletteFocus $search
    [System.Windows.Forms.SendKeys]::SendWait('ergonomie')
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    $stableQuery = 0
    do {
        if ((Read-ComboValue $search) -eq 'ergonomie') { $stableQuery++ } else { $stableQuery = 0 }
        if ($stableQuery -ge 2) { break }
        Start-Sleep -Milliseconds 150
    } while ([DateTime]::UtcNow -lt $deadline)
    if ($stableQuery -lt 2) { throw 'The command palette did not receive the search text.' }
    # The accessible option name includes its visible settings hint.
    [void](Find-CommandControl 'Ergonomie Paramètres' @($buttonType, [System.Windows.Automation.ControlType]::ListItem))
    [System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
    Focus-Control 'Ergonomie' $tabType
    Assert-TabFocus 'Ergonomie'
}

if ($Mode -eq 'Exercise') {
    $conversation = Find-Control 'Conversation' $buttonType
    $formations = Find-Control 'Formations' $buttonType
    Focus-Control 'Votre message au tuteur' $editType
    [System.Windows.Forms.SendKeys]::SendWait('Je veux apprendre le Go')
    [void](Find-ReadyControl 'Envoyer' $buttonType)
    # The message is typed but never submitted. The test profile is offline.
    Activate-Control $formations
    # Wait for actual page content, not the sidebar button that never leaves.
    [void](Find-Control 'Connecter Tutor' $buttonType)
    Activate-Control $conversation
    Focus-Control 'Votre message au tuteur' $editType
    [void](Find-ReadyControl 'Envoyer' $buttonType)
    [System.Windows.Forms.SendKeys]::SendWait('{TAB}')
    Assert-ControlFocus 'Envoyer' $buttonType

    Exercise-CommandPalette
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
    Focus-Control 'Your message to the tutor' $editType
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
    Focus-Control 'Votre message au tuteur' $editType
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
    commandPaletteKeyboardNavigation = ($Mode -eq 'Exercise')
    languageSwitchFrEnFr = ($Mode -eq 'Exercise')
    themeSwitchLightDark = $true
    persistenceAfterProcessRestart = ($Mode -eq 'VerifyPersistence')
    networkRequestSent = $false
    offlineFixtureRequired = $true
    sendBeforeSetup = ($Mode -eq 'Exercise')
    narratorManualAudit = $false
} | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $Report -Encoding UTF8
Write-Host "UI Automation names, keyboard settings, language and themes passed ($Mode)."
