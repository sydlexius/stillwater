// global-setup.js - one-time auth for the a11y smoke tier.
//
// Authenticates ONCE for the whole Playwright run (setup + login via the
// bruno-ci bootstrap) and persists the session into a storageState file that
// every test context loads (see `use.storageState` in playwright.config.js).
//
// Why this exists: /api/v1/auth/setup and /api/v1/auth/login are behind the
// production login brute-force rate limiter (5 req/min/IP, burst 5, shared
// across all auth endpoints). The previous per-spec-file beforeAll login meant
// each spec file spent two of those tokens; with more than two spec files the
// rapid loopback bursts exceeded the burst and the suite failed with a 429
// "too many requests" before any test ran. Logging in exactly once keeps the
// whole run at two auth calls regardless of how many spec files exist, without
// touching the production rate limiter.

import { request } from 'playwright/test';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import fs from 'node:fs';

import { setupAndLogin } from './helpers/bootstrap.js';

const dirname = path.dirname(fileURLToPath(import.meta.url));

// Shared with playwright.config.js (`use.storageState`). Kept under the a11y
// test dir and gitignored.
export const STORAGE_STATE = path.join(dirname, '.auth', 'state.json');

const BASE_URL = process.env.SW_TEST_URL
  || `http://127.0.0.1:${process.env.SW_PORT || '1973'}`;

export default async function globalSetup() {
  fs.mkdirSync(path.dirname(STORAGE_STATE), { recursive: true });

  const ctx = await request.newContext({ baseURL: BASE_URL });
  try {
    // setupAndLogin receives the Set-Cookie headers on the request context;
    // storageState() then serializes those cookies (session + csrf) for reuse.
    await setupAndLogin(ctx);
    await ctx.storageState({ path: STORAGE_STATE });
  } finally {
    await ctx.dispose();
  }
}
