import { defineConfig } from '@playwright/test';

// dashboard-e2e suite (OA-40): one deterministic run driven by
// scripts/dashboard_e2e.sh — run-wide stub fixtures (started before the app,
// consumed via E2E_* env), a seeded "bootstrap" tenant, and per-role stored
// sessions produced by global.setup.ts through the real login form.
// workers=1, no retries, no sleeps anywhere in the suite.

export default defineConfig({
  testDir: './tests/dashboard',
  timeout: 120_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  retries: 0,
  workers: 1,
  reporter: 'list',
  use: {
    baseURL: process.env.BASE_URL,
    headless: true,
    screenshot: 'only-on-failure',
    viewport: { width: 1440, height: 900 },
  },
  projects: [
    {
      name: 'setup',
      testMatch: 'global.setup.ts',
      use: { storageState: { cookies: [], origins: [] } },
    },
    {
      name: 'admin',
      dependencies: ['setup'],
      use: { storageState: 'tests/dashboard/.auth/admin.json' },
      testMatch: ['nav.spec.ts', 'flows.admin.spec.ts', 'keyboard.spec.ts', 'screenshots.spec.ts', 'legacy.*.spec.ts'],
    },
    {
      name: 'manager',
      dependencies: ['setup'],
      testMatch: ['nav.spec.ts', 'flows.manager.spec.ts', 'keyboard.spec.ts', 'screenshots.spec.ts'],
      use: { storageState: 'tests/dashboard/.auth/manager.json' },
    },
    {
      name: 'member',
      dependencies: ['setup'],
      testMatch: [
        'nav.spec.ts',
        'flows.chat-search.spec.ts',
        'keyboard.spec.ts',
        'screenshots.spec.ts',
      ],
      use: { storageState: 'tests/dashboard/.auth/member.json' },
    },
  ],
});
