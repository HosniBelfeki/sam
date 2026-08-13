// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

const { test, expect } = require('@playwright/test');

const ADMIN_TOKEN = process.env.SAM_ADMIN_TOKEN || 'ui-test-admin-token';

// Tests run serially with a single worker, so one collector is enough.
let jsFailures = [];

// The landing page deliberately probes an authenticated endpoint before login,
// so a 401 and its handler's console.error are expected, not defects.
const EXPECTED_NOISE = [
  'Unauthorized. Please login again.',
  'Failed to load resource: the server responded with a status of 401',
];

// The console is hand-written vanilla JS with no build step, so a renamed element
// id or a typo only surfaces at runtime. Fail the test on any uncaught error.
test.beforeEach(async ({ page }) => {
  jsFailures = [];
  page.on('pageerror', (err) => jsFailures.push(`uncaught: ${err.message}`));
  page.on('console', (msg) => {
    if (msg.type() !== 'error') {
      return;
    }
    const text = msg.text();
    if (EXPECTED_NOISE.some((allowed) => text.includes(allowed))) {
      return;
    }
    jsFailures.push(`console.error: ${text}`);
  });
});

test.afterEach(async () => {
  expect(jsFailures, 'console reported JavaScript errors').toEqual([]);
});

async function login(page) {
  await page.goto('/');
  await page.fill('#admin-token-input', ADMIN_TOKEN);
  await page.click('button:has-text("Login as Admin")');
  await expect(page.locator('#app-container')).toBeVisible();
}

test('admin can log in and reach the bootstrap token form', async ({ page }) => {
  await login(page);

  await page.click('.nav-item[data-target="bootstrap"]');
  await expect(page.locator('#view-bootstrap')).toBeVisible();
  await expect(page.locator('#form-generate-token')).toBeVisible();

  // Owner ID is admin-only and optional.
  await expect(page.locator('#token-owner')).toBeVisible();
  const required = await page.locator('#token-owner').evaluate((el) => el.required);
  expect(required).toBe(false);
});

test('generating a token shows a copyable value and lists it', async ({ page }) => {
  await login(page);
  await page.click('.nav-item[data-target="bootstrap"]');

  await expect(page.locator('#token-result')).toBeHidden();

  await page.selectOption('#token-role', 'sam:role:node');
  await page.fill('#token-desc', 'playwright smoke');
  await page.click('#form-generate-token button[type="submit"]');

  const tokenField = page.locator('#token-result-value');
  await expect(page.locator('#token-result')).toBeVisible();
  await expect(tokenField).toHaveValue(/^sam-bt-[0-9a-f]{32}$/);

  const token = await tokenField.inputValue();
  await expect(page.locator('#token-result-owner')).toHaveText('root-admin');
  await expect(page.locator('#token-result-cmd')).toContainText(token);

  // The copy affordance is the whole point of the panel; prove it works.
  await page.click('#token-copy-btn');
  await expect(page.locator('#token-copy-btn')).toHaveText('Copied');
  const clipboard = await page.evaluate(() => navigator.clipboard.readText());
  expect(clipboard).toBe(token);

  // The new token must appear in the active token list after the refresh.
  await expect(page.locator('#table-bootstrap')).toContainText('root-admin');
  await expect(page.locator('#table-bootstrap')).toContainText('sam:role:node');
});

test('omitting the owner attributes the token to the session user', async ({ page }) => {
  await login(page);
  await page.click('.nav-item[data-target="bootstrap"]');

  await expect(page.locator('#token-owner')).toHaveValue('');
  await page.click('#form-generate-token button[type="submit"]');

  await expect(page.locator('#token-result-owner')).toHaveText('root-admin');
});
