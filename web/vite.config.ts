import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'path';

const apiTarget = process.env.VITE_DIFIT_API_URL || 'http://localhost:4966';

export default defineConfig({
  plugins: [react()],
  root: 'src/client',
  publicDir: '../../public',
  build: {
    // Embedded into the Go binary by internal/web.
    outDir: '../../../internal/web/assets/dist',
    emptyOutDir: true,
  },
  resolve: {
    alias: {
      '@': resolve(__dirname, 'src'),
      '@/types': resolve(__dirname, 'src/types'),
    },
  },
  css: {
    postcss: './postcss.config.js',
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
});
