import { defineConfig } from '@playwright/test';

// E2E_READS=worker reads every paste from the Worker (see fixtures.ts).
// That run keeps its own results and report, so it never clears the other's.
const suffix = process.env.E2E_READS === 'worker' ? '-worker' : '';

export default defineConfig({
  testDir: './tests',
  globalSetup: './global-setup.ts',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  // A cold fetch of a renderer's own libraries has to fit, not just a paint.
  timeout: 90_000,
  expect: { timeout: 30_000 },
  outputDir: `test-results${suffix}`,
  reporter: [['list'], ['html', { open: 'never', outputFolder: `playwright-report${suffix}` }]],
  use: {
    viewport: { width: 1280, height: 900 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium', use: { browserName: 'chromium' } },
    { name: 'webkit', use: { browserName: 'webkit' } },
  ],
});
