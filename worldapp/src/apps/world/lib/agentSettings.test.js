import test from 'node:test';
import assert from 'node:assert/strict';
import { providerPatch, resolvedModel, agentSaveBody, request } from './agentSettings.js';

const form = () => ({ name: ' Local ', base_url: ' https://local.example/v1 ', models: ' cheap\nstrong,cheap ', timeout_seconds: 300, max_parallel: 2, api_key: '', clear_api_key: false, extra_body: '', url_strict: false });

test('Provider patch omits blank credentials and advanced parameters', () => {
  const result = providerPatch(form());
  assert.equal(result.base_url, 'https://local.example/v1');
  assert.deepEqual(result.models, ['cheap', 'strong']);
  assert.ok(!Object.hasOwn(result, 'api_key'));
  assert.ok(!Object.hasOwn(result, 'extra_body'));
  assert.ok(!Object.hasOwn(result, 'clear_api_key'));
});
test('explicit replacement and clear are distinct; legacy fields are not overwritten', () => {
  assert.equal(providerPatch({ ...form(), api_key: ' new-key ' }).api_key, 'new-key');
  assert.equal(providerPatch({ ...form(), clear_api_key: true }).clear_api_key, true);
  assert.throws(() => providerPatch({ ...form(), clear_api_key: true, api_key: 'new' }));
  const legacy = providerPatch({ ...form(), extra_body: '{"enable_thinking":false}' }, true);
  assert.ok(!Object.hasOwn(legacy, 'models'));
  assert.ok(!Object.hasOwn(legacy, 'max_parallel'));
  assert.deepEqual(legacy.extra_body, { enable_thinking: false });
  assert.throws(() => providerPatch({ ...form(), extra_body: '[]' }));
});
test('character models inherit tiers or override them; saves keep revisions and dormant actors', () => {
  const config = { schema_version: 1, revision: 7, tiers: { core: { provider_id: 'a', model: 'strong', max_tokens: 4000 } }, characters: { A: { importance: 'core' }, D: { importance: 'core', model_override: { provider_id: 'b', model: 'cheap' } } } };
  assert.equal(resolvedModel(config, 'A').model, 'strong');
  assert.equal(resolvedModel(config, 'D').provider_id, 'b');
  const body = agentSaveBody(config, 12);
  assert.equal(body.registry_revision, 12);
  assert.deepEqual(body.config, config);
  body.config.characters.A.importance = 'background';
  assert.equal(config.characters.A.importance, 'core');
});
test('requests bind Unicode world names and surface conflicts without mutating caller draft', async () => {
  const oldFetch = globalThis.fetch;
  try {
    globalThis.fetch = async (url) => {
      assert.equal(new URL(url, 'http://local').searchParams.get('world'), '白室 协议');
      return { ok: false, status: 409, json: async () => ({ error: '世界已切换' }) };
    };
    await assert.rejects(request('/api/world/agents/config', { world: '白室 协议' }), e => e.status === 409 && e.message === '世界已切换');
  } finally { globalThis.fetch = oldFetch; }
});
