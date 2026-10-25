# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
[CmdletBinding()]
param([Parameter(Mandatory=$true)][long]$NativeHandle, [Parameter(Mandatory=$true)][string]$Report)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
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
function Find-Control([string]$Name, [System.Windows.Automation.ControlType]$Type) {
    $condition = New-Object System.Windows.Automation.AndCondition(
        (New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::NameProperty, $Name)),
        (New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::ControlTypeProperty, $Type))
    )
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $element = $root.FindFirst([System.Windows.Automation.TreeScope]::Descendants, $condition)
        if ($null -ne $element) { return $element }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Accessible control missing: $Name ($Type)"
}
$button = [System.Windows.Automation.ControlType]::Button
$conversation = Find-Control 'Conversation' $button
$formations = Find-Control 'Formations' $button
$composer = Find-Control 'Votre message au tuteur' ([System.Windows.Automation.ControlType]::Edit)
if (-not $composer.Current.IsKeyboardFocusable) { throw 'The composer cannot receive keyboard focus.' }
$composer.SetFocus()
[System.Windows.Forms.SendKeys]::SendWait('Je veux apprendre le Go')
Start-Sleep -Milliseconds 500
$send = Find-Control 'Envoyer' $button
if (-not $send.Current.IsEnabled) { throw 'Typing did not enable sending before setup.' }
# Navigation uses the same UI Automation patterns as assistive technology.
([System.Windows.Automation.InvokePattern]$formations.GetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern)).Invoke()
[void](Find-Control 'Formations' $button)
([System.Windows.Automation.InvokePattern]$conversation.GetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern)).Invoke()
$composer = Find-Control 'Votre message au tuteur' ([System.Windows.Automation.ControlType]::Edit)
$composer.SetFocus()
[System.Windows.Forms.SendKeys]::SendWait('{TAB}')
Start-Sleep -Milliseconds 200
$focused = [System.Windows.Automation.AutomationElement]::FocusedElement
if ($null -eq $focused -or -not $focused.Current.IsKeyboardFocusable) { throw 'Keyboard navigation lost accessible focus.' }
@{ platform='Windows'; uiaNames=$true; keyboardInput=$true; keyboardNavigation=$true; sendBeforeSetup=$true; networkRequestSent=$false; narratorManualAudit=$false } | ConvertTo-Json | Set-Content -LiteralPath $Report -Encoding UTF8
Write-Host 'UI Automation names, keyboard input and navigation passed.'
