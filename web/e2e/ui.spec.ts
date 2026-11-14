import { expect, Page, test } from '@playwright/test';

const TOKEN = process.env.TF_TOKEN ?? 'demo-api-token-123';

/** Collects console errors and failed requests so every test can assert a clean run. */
function watch(page: Page) {
  const bad: string[] = [];
  page.on('console', m => { if (m.type() === 'error') bad.push(`console: ${m.text()}`); });
  page.on('pageerror', e => bad.push(`pageerror: ${e.message}`));
  page.on('requestfailed', r => bad.push(`failed: ${r.url()}`));
  page.on('response', r => { if (r.status() >= 400) bad.push(`http ${r.status()}: ${r.url()}`); });
  return bad;
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript(t => sessionStorage.setItem('tf_token', t), TOKEN);
});

test('token gate: protected API asks for a token, a wrong one is rejected, the right one works', async ({ page }) => {
  await page.addInitScript(() => sessionStorage.removeItem('tf_token'));
  await page.goto('/');
  await expect(page.getByText('This server requires an API token')).toBeVisible();
  await page.getByLabel('API token').fill('wrong');
  await page.getByRole('button', { name: 'Continue' }).click();
  await expect(page.getByText('This server requires an API token')).toBeVisible();
  await page.getByLabel('API token').fill(TOKEN);
  await page.getByRole('button', { name: 'Continue' }).click();
  await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible();
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page.getByLabel('API token')).toBeVisible();
});

test('every page is reachable from the sidebar and loads without console or network errors', async ({ page }) => {
  const bad = watch(page);
  await page.goto('/');
  for (const [link, heading] of [['Overview', 'Overview'], ['Services', 'Services'], ['Dependencies', 'Dependencies'], ['Traces', 'Traces'],
    ['Incidents', 'Incidents'], ['Replay', 'Counterfactual replay'], ['Memory graph', 'Incident memory'], ['Timeline', 'Timeline'],
    ['Reports', 'Reports'], ['Failure lab', 'Failure lab'], ['Settings', 'Settings']]) {
    await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: link }).click();
    await expect(page.getByRole('heading', { name: heading, level: 1 })).toBeVisible();
    await page.waitForLoadState('networkidle');
  }
  expect(bad).toEqual([]);
});

test('services: filter by name and open a service', async ({ page }) => {
  await page.goto('/#/services');
  await expect(page.getByRole('link', { name: 'payment' })).toBeVisible();
  await page.getByLabel('Filter services by name').fill('pay');
  await expect(page.getByRole('link', { name: 'gateway' })).toHaveCount(0);
  await page.getByRole('link', { name: 'payment' }).click();
  await expect(page.getByRole('heading', { name: 'payment', level: 1 })).toBeVisible();
  await expect(page.getByText('Called by')).toBeVisible();
  await page.goto('/#/services');
  await page.getByLabel('Filter services by name').fill('zzz-nothing');
  await expect(page.getByText('No matching services')).toBeVisible();
});

test('incident: list, detail tabs, status change, note, investigation ranks payment first', async ({ page }) => {
  const bad = watch(page);
  await page.goto('/#/incidents');
  await expect(page.getByRole('link', { name: /Dependency degradation/ }).first()).toBeVisible();
  await page.goto('/#/incidents/1'); // the first payment-latency incident of a fresh stack (scripts/e2e.sh creates it)
  await expect(page.getByText('Signals')).toBeVisible();
  await page.getByRole('tab', { name: 'Investigation' }).click();
  await expect(page.getByText('integrity verified')).toBeVisible();
  await expect(page.locator('summary', { hasText: '#1' })).toContainText('payment');
  await page.getByRole('tab', { name: 'Notes' }).click();
  const note = `playwright note ${Date.now()}`;
  await page.getByLabel('Add a note').fill(note);
  await page.getByRole('button', { name: 'Add note' }).click();
  await expect(page.getByText(note)).toBeVisible();
  await page.getByRole('tab', { name: 'Similar incidents' }).click();
  await expect(page.getByText('simulated example').first()).toBeVisible();
  expect(bad).toEqual([]);
});

test('incident reports download with the API token', async ({ page }) => {
  await page.goto('/#/reports');
  for (const ext of ['md', 'html', 'json']) {
    const [dl] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: `.${ext}`, exact: true }).first().click()]);
    expect(dl.suggestedFilename()).toMatch(new RegExp(`\\.${ext}$`));
    const path = await dl.path();
    expect((await import('node:fs')).statSync(path!).size).toBeGreaterThan(1000);
  }
});

test('replay: run a counterfactual and see labelled simulated results; invalid input is reported', async ({ page }) => {
  const bad = watch(page);
  await page.goto('/#/replay?incident=1');
  await expect(page.getByText('Results are')).toBeVisible();
  await expect(page.getByLabel('Service')).toHaveValue('payment'); // defaults to the top-ranked origin
  await page.getByRole('button', { name: 'Run replay' }).click();
  await expect(page.getByText('Counterfactual').first()).toBeVisible();
  await expect(page.getByText('simulated').first()).toBeVisible();
  await expect(page.getByText('Replayed').first()).toBeVisible();
  // retry_policy with no caller must be rejected by the server and shown, not swallowed
  await page.getByLabel('Intervention').selectOption('retry_policy');
  await page.getByRole('button', { name: 'Run replay' }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  expect(bad.filter(b => !b.startsWith('http 400') && !b.includes('Failed to load resource'))).toEqual([]);
});

test('memory graph: load demo records, filter, select a node, compare records', async ({ page }) => {
  await page.goto('/#/memory');
  await page.getByRole('button', { name: 'Load demo records' }).click();
  await expect(page.getByRole('img', { name: 'Incident memory graph' })).toBeVisible();
  const nodes = page.locator('.gnode');
  const before = await nodes.count();
  expect(before).toBeGreaterThan(3);
  await nodes.first().click();
  await expect(page.locator('.gnode.sel')).toHaveCount(1);
  await expect(page.locator('.gnode.dim').first()).toBeAttached();
  await nodes.first().click(); // deselect
  await expect(page.locator('.gnode.sel')).toHaveCount(0);
  await page.getByLabel('Hide simulated').check();
  expect(await nodes.count()).toBeLessThan(before);
  await page.getByLabel('Hide simulated').uncheck();
  await page.getByLabel('Filter graph').fill('payment');
  expect(await nodes.count()).toBeGreaterThan(0);
  await page.getByRole('tab', { name: 'Records' }).click();
  await expect(page.getByText('simulated example').first()).toBeVisible();
  await page.getByRole('tab', { name: 'Compare' }).click();
  const opts = await page.getByLabel('Record A').locator('option').allTextContents();
  expect(opts.length).toBeGreaterThan(2);
  await page.getByLabel('Record A').selectOption({ index: 1 });
  await page.getByLabel('Record B').selectOption({ index: 1 });
  await expect(page.getByText('Choose two different records.')).toBeVisible();
  await page.getByLabel('Record B').selectOption({ index: 2 });
  await expect(page.getByText('% overlap')).toBeVisible();
});

test('lab: shows targets and an enabled start button', async ({ page }) => {
  await page.goto('/#/lab');
  await expect(page.getByRole('heading', { name: 'Targets' })).toBeVisible();
  await expect(page.getByText('reachable').first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Start experiment' })).toBeEnabled();
});

test('timeline: shows events and filters by service', async ({ page }) => {
  await page.goto('/#/timeline');
  await expect(page.locator('.item').first()).toBeVisible();
  await page.getByLabel('Filter by service').fill('no-such-service');
  await expect(page.getByText('No events')).toBeVisible();
});

test('settings: shows auth state and rejects an invalid detection config', async ({ page }) => {
  await page.goto('/#/settings');
  await expect(page.getByText('required').first()).toBeVisible();
  const f = page.getByLabel('window_seconds');
  await f.fill('0');
  await page.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByRole('alert')).toBeVisible();
  await page.getByRole('button', { name: 'Restore defaults' }).click();
  await expect(page.getByText('Saved.')).toBeVisible();
});

test('empty, loading and error states', async ({ page }) => {
  await page.route('**/api/v1/incidents?*', r => r.fulfill({ json: { incidents: [], total: 0 } }));
  await page.goto('/#/incidents');
  await expect(page.getByText('No incidents')).toBeVisible();
  await page.unroute('**/api/v1/incidents?*');
  await page.route('**/api/v1/services?*', r => r.fulfill({ status: 500, json: { error: { message: 'boom' } } }));
  await page.goto('/#/services');
  await expect(page.getByRole('alert')).toContainText('boom');
  await page.unroute('**/api/v1/services?*');
  await page.route('**/api/v1/timeline?*', async r => { await new Promise(x => setTimeout(x, 800)); await r.continue(); });
  await page.goto('/#/timeline');
  await expect(page.getByLabel('Loading')).toBeVisible();
});

test('mobile layout: sidebar collapses behind the menu button', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await page.goto('/');
  await expect(page.getByRole('heading', { name: 'Overview', level: 1 })).toBeVisible();
  await expect(page.getByText('requests / s')).toBeVisible();
  await page.waitForLoadState('networkidle');
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Incidents' }).click();
  await expect(page.getByRole('heading', { name: 'Incidents', level: 1 })).toBeVisible();
});
