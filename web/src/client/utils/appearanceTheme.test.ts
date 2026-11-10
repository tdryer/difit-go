import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  APPEARANCE_STORAGE_KEY,
  applyResolvedTheme,
  bootstrapAppearanceTheme,
  getResolvedTheme,
  resolveThemePreference,
} from './appearanceTheme';

const setMatchMedia = (matches: boolean) => {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    writable: true,
    value: vi.fn().mockImplementation(() => ({
      matches,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });
};

describe('appearanceTheme', () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute('data-color-vision');
    document.documentElement.removeAttribute('data-theme');
    document.documentElement.removeAttribute('style');
    document.body.removeAttribute('style');
    setMatchMedia(true);
  });

  it('resolves an explicit theme preference', () => {
    expect(resolveThemePreference('light', 'dark')).toBe('light');
    expect(resolveThemePreference('dark', 'light')).toBe('dark');
  });

  it('bootstraps the saved light theme before app mount', () => {
    localStorage.setItem(
      APPEARANCE_STORAGE_KEY,
      JSON.stringify({
        theme: 'light',
      }),
    );

    expect(bootstrapAppearanceTheme()).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(document.documentElement.style.getPropertyValue('--color-github-bg-primary')).toBe(
      '#ffffff',
    );
  });

  it.each([
    { theme: 'dark' as const, addition: '#12261e', deletion: '#25171c' },
    { theme: 'light' as const, addition: '#dafbe1', deletion: '#ffebe9' },
  ])(
    'uses subtle GitHub line backgrounds when bootstrapping $theme',
    ({ theme, addition, deletion }) => {
      localStorage.setItem(APPEARANCE_STORAGE_KEY, JSON.stringify({ theme }));

      expect(bootstrapAppearanceTheme()).toBe(theme);
      expect(document.documentElement.style.getPropertyValue('--color-diff-addition-bg')).toBe(
        addition,
      );
      expect(document.documentElement.style.getPropertyValue('--color-diff-deletion-bg')).toBe(
        deletion,
      );
    },
  );

  it('updates line backgrounds when switching themes and color vision modes', () => {
    applyResolvedTheme('dark', 'deuteranopia');
    expect(document.documentElement.style.getPropertyValue('--color-diff-addition-bg')).toBe(
      '#0c2d6b',
    );
    expect(document.documentElement.style.getPropertyValue('--color-diff-deletion-bg')).toBe(
      '#5a3600',
    );

    applyResolvedTheme('light');
    expect(document.documentElement.style.getPropertyValue('--color-diff-addition-bg')).toBe(
      '#dafbe1',
    );
    expect(document.documentElement.style.getPropertyValue('--color-diff-deletion-bg')).toBe(
      '#ffebe9',
    );

    applyResolvedTheme('dark');
    expect(document.documentElement.style.getPropertyValue('--color-diff-addition-bg')).toBe(
      '#12261e',
    );
    expect(document.documentElement.style.getPropertyValue('--color-diff-deletion-bg')).toBe(
      '#25171c',
    );
  });

  it('bootstraps the saved color vision mode before app mount', () => {
    localStorage.setItem(
      APPEARANCE_STORAGE_KEY,
      JSON.stringify({
        colorVision: 'deuteranopia',
        theme: 'dark',
      }),
    );

    expect(bootstrapAppearanceTheme()).toBe('dark');
    expect(document.documentElement.getAttribute('data-color-vision')).toBe('deuteranopia');
    expect(document.documentElement.style.getPropertyValue('--color-diff-addition-bg')).toBe(
      '#0c2d6b',
    );
  });

  it('resolves auto theme from system preference when no attribute is present', () => {
    localStorage.setItem(
      APPEARANCE_STORAGE_KEY,
      JSON.stringify({
        theme: 'auto',
      }),
    );
    setMatchMedia(false);

    expect(getResolvedTheme()).toBe('light');
    expect(bootstrapAppearanceTheme()).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });
});
