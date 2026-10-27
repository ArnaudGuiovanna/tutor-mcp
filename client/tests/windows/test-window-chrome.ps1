# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
# Dot-sourced by test-accessibility.ps1, after the isolated offline profile check.
# Exercise native OS state through the actual WebView controls, not a mocked bridge.
Add-Type @'
using System;
using System.Runtime.InteropServices;
public static class TutorWindowNative {
 [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
 [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
 [StructLayout(LayoutKind.Sequential)] public struct MOUSEINPUT { public int dx, dy; public uint mouseData, dwFlags, time; public UIntPtr dwExtraInfo; }
 [StructLayout(LayoutKind.Sequential)] public struct INPUT { public uint type; public MOUSEINPUT mouse; }
 [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr window, IntPtr after, int x, int y, int width, int height, uint flags);
 [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr window, out RECT rect);
 [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr window, ref POINT point);
 [DllImport("user32.dll")] public static extern bool IsIconic(IntPtr window);
 [DllImport("user32.dll")] public static extern bool IsZoomed(IntPtr window);
 [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr window, int command);
 [DllImport("user32.dll")] public static extern uint GetDpiForWindow(IntPtr window);
 [DllImport("user32.dll")] public static extern IntPtr SetThreadDpiAwarenessContext(IntPtr context);
 [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
 [DllImport("user32.dll")] public static extern uint SendInput(uint count, INPUT[] inputs, int size);
 public static void Mouse(uint flags) {
  INPUT input = new INPUT(); input.type = 0; input.mouse.dwFlags = flags;
  if (SendInput(1, new INPUT[] {input}, Marshal.SizeOf(typeof(INPUT))) != 1) throw new Exception("SendInput failed for window gesture.");
 }
}
'@
# UI Automation rectangles are physical pixels. Align the Win32 observations.
[void][TutorWindowNative]::SetThreadDpiAwarenessContext([IntPtr](-4))
function Read-WindowRect {
    $rect = New-Object TutorWindowNative+RECT
    if (-not [TutorWindowNative]::GetWindowRect([IntPtr]$NativeHandle, [ref]$rect)) { throw 'Cannot observe the actual window rectangle.' }
    return $rect
}
function Wait-WindowState([scriptblock]$Condition, [string]$Description) {
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    do {
        if (& $Condition) { return }
        Start-Sleep -Milliseconds 100
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Window action did not take effect: $Description"
}
function Move-WindowPointer([int]$X, [int]$Y) {
    if (-not [TutorWindowNative]::SetCursorPos($X, $Y)) { throw 'Cannot move the qualification pointer.' }
}
function Drag-WindowPointer([int]$X, [int]$Y, [int]$DX, [int]$DY) {
    Move-WindowPointer $X $Y
    [TutorWindowNative]::Mouse(2)
    try {
        Start-Sleep -Milliseconds 200
        for ($step = 1; $step -le 8; $step++) {
            Move-WindowPointer ($X + [int]($DX * $step / 8)) ($Y + [int]($DY * $step / 8))
            Start-Sleep -Milliseconds 80
        }
    } finally { [TutorWindowNative]::Mouse(4) }
}
function Save-WindowCapture([string]$Path) {
    Add-Type -AssemblyName System.Drawing
    $parent = Split-Path -Parent $Path
    if ($parent -and -not (Test-Path -LiteralPath $parent)) { [void](New-Item -ItemType Directory -Path $parent) }
    $rect = Read-WindowRect
    $bitmap = [System.Drawing.Bitmap]::new(($rect.Right - $rect.Left), ($rect.Bottom - $rect.Top))
    $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
    try {
        $graphics.CopyFromScreen($rect.Left, $rect.Top, 0, 0, $bitmap.Size)
        $bitmap.Save($Path, [System.Drawing.Imaging.ImageFormat]::Png)
    } finally { $graphics.Dispose(); $bitmap.Dispose() }
}
function Exercise-WindowChrome {
    [void](Find-ReadyControl 'Réduire la fenêtre' $buttonType)
    $work = [System.Windows.Forms.Screen]::FromHandle([IntPtr]$NativeHandle).WorkingArea
    if ($work.Width -lt 720 -or $work.Height -lt 560) { throw 'The Windows qualification desktop is smaller than the supported test viewport.' }
    if (-not [TutorWindowNative]::SetWindowPos([IntPtr]$NativeHandle, [IntPtr]::Zero, ($work.Left+40), ($work.Top+40), [Math]::Min(1100,$work.Width-80), [Math]::Min(780,$work.Height-80), 4)) { throw 'Cannot position the isolated test window inside the working area.' }
    Start-Sleep -Milliseconds 300
    $minimize = Find-ReadyControl 'Réduire la fenêtre' $buttonType
    [void](Find-ReadyControl 'Agrandir la fenêtre' $buttonType)
    [void](Find-ReadyControl 'Fermer la fenêtre' $buttonType)
    $before = Read-WindowRect
    $clientOrigin = New-Object TutorWindowNative+POINT
    if (-not [TutorWindowNative]::ClientToScreen([IntPtr]$NativeHandle, [ref]$clientOrigin)) { throw 'Cannot observe the native client origin.' }
    $scale = [TutorWindowNative]::GetDpiForWindow([IntPtr]$NativeHandle) / 96.0
    $captionInset = $clientOrigin.Y - $before.Top
    # Tao retains some Win32 caption flags for resizing. Its real non-client
    # geometry is the evidence: only the shadow/border may precede the WebView.
    if ($captionInset -gt (12 * $scale) -or $captionInset -lt 0) { throw "A native title bar still occupies $captionInset pixels above the client." }
    if (($minimize.Current.BoundingRectangle.Top - $clientOrigin.Y) -gt (8 * $scale)) { throw 'Custom controls are not at the top of the client surface.' }

    Activate-Control $minimize
    Wait-WindowState { [TutorWindowNative]::IsIconic([IntPtr]$NativeHandle) } 'minimise through the custom button'
    [void][TutorWindowNative]::ShowWindow([IntPtr]$NativeHandle, 9)
    Wait-WindowState { -not [TutorWindowNative]::IsIconic([IntPtr]$NativeHandle) } 'restore from the taskbar'
    [void][TutorFocus]::SetForegroundWindow([IntPtr]$NativeHandle)
    Focus-Control 'Agrandir la fenêtre' $buttonType
    [System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
    Wait-WindowState { [TutorWindowNative]::IsZoomed([IntPtr]$NativeHandle) } 'maximise with the keyboard'
    Focus-Control 'Restaurer la fenêtre' $buttonType
    [System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
    Wait-WindowState { -not [TutorWindowNative]::IsZoomed([IntPtr]$NativeHandle) } 'restore with the keyboard'

    $before = Read-WindowRect
    $control = Find-ReadyControl 'Réduire la fenêtre' $buttonType
    $bounds = $control.Current.BoundingRectangle
    Drag-WindowPointer ([int]($bounds.Left - 120 * $scale)) ([int]($bounds.Top + $bounds.Height / 2)) 24 16
    Wait-WindowState { $rect = Read-WindowRect; [Math]::Abs($rect.Left - $before.Left) -ge 20 -and [Math]::Abs($rect.Top - $before.Top) -ge 10 } 'drag the custom title surface'

    # Double-click the same draggable surface, then restore via its custom control.
    $control = Find-ReadyControl 'Réduire la fenêtre' $buttonType
    $bounds = $control.Current.BoundingRectangle
    Move-WindowPointer ([int]($bounds.Left - 120 * $scale)) ([int]($bounds.Top + $bounds.Height / 2))
    [TutorWindowNative]::Mouse(2)
    Start-Sleep -Milliseconds 40
    [TutorWindowNative]::Mouse(4)
    Start-Sleep -Milliseconds 80
    [TutorWindowNative]::Mouse(2)
    Start-Sleep -Milliseconds 40
    [TutorWindowNative]::Mouse(4)
    Wait-WindowState { [TutorWindowNative]::IsZoomed([IntPtr]$NativeHandle) } 'double-click the custom title surface'
    Activate-Control (Find-ReadyControl 'Restaurer la fenêtre' $buttonType)
    Wait-WindowState { -not [TutorWindowNative]::IsZoomed([IntPtr]$NativeHandle) } 'restore after double-click'

    $before = Read-WindowRect
    Drag-WindowPointer ($before.Right - 2) ($before.Bottom - 2) -60 -40
    Wait-WindowState { $rect = Read-WindowRect; ($before.Right - $before.Left) - ($rect.Right - $rect.Left) -ge 20 -and ($before.Bottom - $before.Top) - ($rect.Bottom - $rect.Top) -ge 15 } 'resize using the native edge'
    return @{ nativeCaptionAbsent = $true; nativeCaptionInsetPixels = $captionInset; customMinimize = $true; customMaximizeRestore = $true; titlebarDrag = $true; titlebarDoubleClick = $true; edgeResize = $true }
}
