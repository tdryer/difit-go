import '@testing-library/jest-dom';
import { vi } from 'vitest';

// Mock fetch globally for component tests
global.fetch = vi.fn();

// Mock console.error to suppress error logs during tests
global.console.error = vi.fn();

// Mock window.getComputedStyle
Object.defineProperty(window, 'getComputedStyle', {
  value: () => ({
    getPropertyValue: () => '',
  }),
});

// Global test utilities
export const mockFetch = (response: any, revisionsResponse?: any) => {
  (global.fetch as any).mockImplementation((url: string) => {
    // Handle /api/revisions endpoint
    if (url.includes('/api/revisions')) {
      return Promise.resolve({
        ok: revisionsResponse !== null,
        json: async () =>
          revisionsResponse ?? {
            specialOptions: [],
            branches: [],
            commits: [],
          },
      });
    }
    // Default: /api/diff and others
    return Promise.resolve({
      ok: true,
      json: async () => response,
      blob: async () => ({ size: 1024 }),
    });
  });
};

export const mockFetchError = (error: string) => {
  (global.fetch as any).mockRejectedValue(new Error(error));
};
