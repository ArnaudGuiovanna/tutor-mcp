/** True on Apple platforms, where shortcuts use the Command key. */
export function isApplePlatform(): boolean {
  if (typeof navigator === 'undefined') return false;
  const hinted = (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform;
  return /mac|iphone|ipad|ios/i.test(hinted || navigator.platform || '');
}

/** The displayed modifier of keyboard shortcuts: « ⌘ » on macOS, « Ctrl » elsewhere. */
export function shortcutModifier(): string {
  return isApplePlatform() ? '⌘' : 'Ctrl';
}
