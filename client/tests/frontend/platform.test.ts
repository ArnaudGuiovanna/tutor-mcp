import { afterEach, describe, expect, it, vi } from 'vitest';
import { shortcutModifier } from '../../src/platform';

describe('Raccourcis clavier affichés', () => {
  afterEach(() => vi.unstubAllGlobals());
  it('affiche ⌘ sur macOS et Ctrl ailleurs', () => {
    vi.stubGlobal('navigator', { platform: 'MacIntel' });
    expect(shortcutModifier()).toBe('⌘');
    vi.stubGlobal('navigator', { platform: '', userAgentData: { platform: 'macOS' } });
    expect(shortcutModifier()).toBe('⌘');
    vi.stubGlobal('navigator', { platform: 'Win32', userAgentData: { platform: 'Windows' } });
    expect(shortcutModifier()).toBe('Ctrl');
  });
});
