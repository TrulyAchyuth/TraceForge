import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', sourcemap: false },
  server: { port: 5173, proxy: { '/api': 'http://localhost:8080', '/healthz': 'http://localhost:8080' } },
  test: { include: ['src/**/*.test.ts'] },
});
