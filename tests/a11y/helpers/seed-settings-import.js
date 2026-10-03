// seed-settings-import.js - fixture for the settings-import dropped-rows
// a11y spec (#3012).
//
// WHY THIS EXISTS
//
// `make test-a11y` boots the server against a BRAND NEW EMPTY DATABASE, so the
// Settings import card has nothing to import and a clean import never renders
// the dropped-rows warning. A conditional skip would report green while
// verifying nothing; the gap is the absent DATA, so this builds an encrypted
// backup file that the importer will PARTIALLY apply, inside the harness.
//
// The envelope scheme mirrors internal/settingsio encryptWithPassphrase:
// PBKDF2-SHA256 (600000 iterations, 16-byte salt) derives a 32-byte key, and
// AES-256-GCM seals the JSON payload. Go's gcm.Seal(nonce, nonce, ...) yields
// nonce || ciphertext || tag, which is what is assembled below.
//
// SIDE EFFECTS ON THE SHARED SERVER ARE NIL ON PURPOSE. Every row in the
// payload is one the importer drops: the bad threshold is rejected, the
// old-name key loses to the current name, the blank-named library and the
// blank-hash token are skipped. Nothing is written, so importing it (twice:
// once through the API to prove the fixture, once through the UI) cannot
// change what other specs see.

import { createCipheriv, pbkdf2Sync, randomBytes } from 'node:crypto';
import { apiFetch } from './api.js';

export const IMPORT_PASSPHRASE = 'a11y-import-passphrase';

// The rejected key, asserted by name in the rendered warning.
export const REJECTED_KEY = 'mbid_revalidate.name_similarity_threshold';

// One of each drop the fixture produces. ConnectionFeaturesIgnored is not in
// the browser fixture because provoking it creates a connection row; the Go
// handler tests cover that counter.
export const EXPECTED_COUNTS = {
  settings_rejected: 1,
  settings_renamed_dropped: 1,
  libraries_skipped: 1,
  api_tokens_skipped: 1,
};

// buildDroppedRowsEnvelope returns the backup file contents as a JSON string.
export function buildDroppedRowsEnvelope(passphrase = IMPORT_PASSPHRASE) {
  const payload = {
    settings: {
      // 0-100 integer key given a fraction: rejected by the validator.
      [REJECTED_KEY]: '0.5',
      // Pre-rename name while the current name is also present: dropped.
      'mbid_revalidate.name_similarity': '80',
    },
    libraries: [{ name: '', path: '/a11y-unused', type: 'regular', source: 'manual' }],
    api_tokens: [{ name: 'a11y-blank-hash', token_hash: '' }],
  };
  const salt = randomBytes(16);
  const nonce = randomBytes(12);
  const key = pbkdf2Sync(passphrase, salt, 600000, 32, 'sha256');
  const cipher = createCipheriv('aes-256-gcm', key, nonce);
  const sealed = Buffer.concat([cipher.update(JSON.stringify(payload), 'utf8'), cipher.final()]);
  const data = Buffer.concat([nonce, sealed, cipher.getAuthTag()]);
  return JSON.stringify({
    version: '1.7',
    app_version: 'a11y-fixture',
    created_at: new Date().toISOString(),
    salt: salt.toString('base64'),
    data: data.toString('base64'),
  });
}

// importViaApi posts the envelope to the real import endpoint (JSON mode) and
// returns the decoded ImportResult. It throws on a non-200 so a broken fixture
// fails here with the server's message instead of as a missing warning later.
export async function importViaApi(request, envelopeJson, passphrase = IMPORT_PASSPHRASE) {
  const resp = await apiFetch(request, 'POST', '/api/v1/settings/import', {
    passphrase,
    envelope: JSON.parse(envelopeJson),
  });
  if (!resp.ok()) {
    throw new Error(`seed: settings import failed: ${resp.status()} ${await resp.text()}`);
  }
  return resp.json();
}
