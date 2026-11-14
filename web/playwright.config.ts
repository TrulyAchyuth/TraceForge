import { defineConfig } from '@playwright/test';

// Runs against a live compose stack: TF_API_TOKEN=... docker-compose -f deploy/docker-compose.yml up -d
// then: BASE_URL=http://localhost:8080 TF_TOKEN=<api token> npm run e2e
export default defineConfig({
  testDir: 'e2e', timeout: 30_000, retries: 0, workers: 1,
  use: { baseURL: process.env.BASE_URL ?? 'http://localhost:8080', headless: true, viewport: { width: 1280, height: 800 } },
});
