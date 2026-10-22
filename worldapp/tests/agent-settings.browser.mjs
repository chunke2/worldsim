// Run after npm run build. Install Playwright externally or set PLAYWRIGHT_MODULE.
// All API calls are intercepted; this never touches a running world or an LLM.
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = fileURLToPath(new URL('../../uiteg/', import.meta.url));
const server = createServer(async (req, res) => {
  try {
    const name = new URL(req.url, 'http://local').pathname;
    const relative = name === '/' ? 'index.html' : name.slice(1);
    const target = path.resolve(root, relative);
    if (!target.startsWith(root)) throw new Error('outside static root');
    const data = await readFile(target);
    res.setHeader('Content-Type', target.endsWith('.js') ? 'text/javascript' : target.endsWith('.css') ? 'text/css' : 'text/html');
    res.end(data);
  } catch { res.writeHead(404).end(); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const base = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch({ headless: true, ...(process.env.BROWSER_CHANNEL ? { channel: process.env.BROWSER_CHANNEL } : {}) });
try {
  const page = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
  const errors = [], writes = [], tests = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('dialog', dialog => dialog.accept());
  await page.addInitScript(() => localStorage.setItem('worldsim.browser', JSON.stringify({ bookmarks: [{ id: 'w', title: '世界模拟', icon: '', address: 'world' }] })));
  let world = '五人白室', conflict = false, revision = 1;
  const providers = {
    a: { name: 'A 服务', protocol: 'openai_compatible', base_url: 'https://a.example/v1', models: ['strong'], has_api_key: true, timeout_seconds: 300, max_parallel: 1 },
    b: { name: 'B 服务', protocol: 'openai_compatible', base_url: 'https://b.example/v1', models: ['cheap'], has_api_key: true, timeout_seconds: 300, max_parallel: 2 },
  };
  const initial = () => ({ schema_version: 1, revision: 0,
    tiers: Object.fromEntries(['core', 'supporting', 'background'].map(t => [t, { provider_id: 'a', model: 'strong', max_tokens: 4000 }])),
    characters: Object.fromEntries(['A', 'B', 'C', 'D', 'E'].map(id => [id, { importance: 'core' }])) });
  let config = initial();
  let scene = { initialized: false };
  const runtimeRows = () => Object.keys(config.characters).map(character_id => ({ character_id, state: 'active', observations: 3, thoughts: 1, latest_observations: 2, prompt_tokens: 100, completion_tokens: 30, last_action: 'wait' }));
  await page.route('**/*', async route => {
    const req = route.request(), url = new URL(req.url());
    if (url.origin !== base) return route.abort();
    if (!url.pathname.startsWith('/api/')) return route.continue();
    const p = url.pathname, method = req.method();
    let body = {}, status = 200;
    if (method !== 'GET') writes.push({ path: p, world: url.searchParams.get('world'), body: req.postDataJSON() });
    if (p === '/api/worlds') body = { worlds: ['五人白室', '另一个世界'].map(name => ({ name, active: name === world, day: 1 })) };
    else if (p === '/api/worlds/select') { world = req.postDataJSON().world; config = initial(); body = { ok: true }; }
    else if (p === '/api/world/state') body = { day: 1, revision: 1, entities: {}, world_level: {} };
    else if (p === '/api/world/loop') body = { running: false };
    else if (p === '/api/novel/config/api') body = { revision: 'legacy', has_api_key: true, base_url: 'https://a.example/v1', model: 'strong' };
    else if (p === '/api/llm/providers') {
      if (method === 'PUT') {
        const u = req.postDataJSON();
        assert.equal(u.revision, String(revision));
        for (const [id, patch] of Object.entries(u.providers || {})) {
          const { api_key, clear_api_key, ...rest } = patch;
          providers[id] = { ...providers[id], ...rest, has_api_key: clear_api_key ? false : !!api_key || providers[id]?.has_api_key };
        }
        revision++;
      }
      body = { ok: true, revision: String(revision), default_provider: 'a', providers };
    } else if (p.endsWith('/test')) { tests.push(p); body = { ok: true, model: 'strong', latency_ms: 1 }; }
    else if (p === '/api/world/agents/config') {
      assert.equal(url.searchParams.get('world'), world);
      if (method === 'PUT') {
        if (conflict) { status = 409; body = { error: '配置版本冲突' }; }
        else { config = req.postDataJSON().config; config.revision++; }
      }
      if (status === 200) body = { ok: true, config, registry_revision: 3, validation_error: '' };
    } else if (p === '/api/world/agents/runtime') body = { ok: true, scene, agents: runtimeRows(), world_revision: 4, diagnostics: {}, last_event_id: 'event:4:test' };
    else if (p.endsWith('/cognition')) body = { ok: true, cognition: { owner: { character_id: 'A' }, records: [{ text: 'OPERATOR-ONLY-PRIVATE-MEMORY' }] } };
    else if (p.endsWith('/audit')) body = { ok: true, audit: [{ status: 'received' }] };
    else if (p === '/api/world/agents/pressures') { assert.equal(method, 'POST'); body = { ok: true, model_calls: 0 }; }
    else if (p === '/api/world/agents') body = { ok: true, registry_revision: 3, config_revision: config.revision, agents: Object.keys(config.characters).map(character_id => ({ character_id, state: character_id === 'E' ? 'dormant' : 'active', autonomous_runtime: false })) };
    await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });
  });
  await page.goto(base);
  await page.getByTitle('世界模拟 （右键删除书签）').click();
  await page.getByRole('button', { name: 'API 服务 / 角色 Agent 设置', exact: true }).click();
  await page.getByText('共 5 个 Agent', { exact: false }).waitFor();
  assert.equal(writes.length, 0, 'opening config must be read only');
  assert.equal(tests.length, 0);
  assert.equal(await page.getByText('OPERATOR-ONLY-PRIVATE-MEMORY').count(), 0);
  await page.getByRole('button', { name: '私人认知', exact: true }).first().click();
  await page.getByText('OPERATOR-ONLY-PRIVATE-MEMORY', { exact: false }).waitFor();
  const inspection = page.getByRole('region', { name: '独立角色运行观察' });
  await inspection.getByRole('button', { name: '关闭', exact: true }).click();
  assert.equal(await page.getByText('OPERATOR-ONLY-PRIVATE-MEMORY').count(), 0);
  await page.getByLabel('运行方式', { exact: false }).selectOption('independent');
  const card = id => page.locator('strong').filter({ hasText: new RegExp(`^${id}$`) }).locator('../..');
  await card('D').getByRole('combobox').selectOption('supporting');
  await card('D').getByRole('checkbox').check();
  await card('D').getByLabel(/^API 服务/).selectOption('b');
  await card('D').getByLabel('模型名称', { exact: true }).fill('cheap');
  await page.getByRole('button', { name: '保存三档与全部人物设置' }).click();
  await page.getByText('配置版本 1', { exact: false }).waitFor();
  const saved = writes.find(w => w.path === '/api/world/agents/config');
  assert.equal(saved.body.registry_revision, 3);
  assert.equal(saved.body.config.revision, 0);
  assert.equal(saved.body.config.characters.D.importance, 'supporting');
  assert.deepEqual(saved.body.config.characters.D.model_override, { provider_id: 'b', model: 'cheap' });
  assert.ok(saved.body.config.characters.E, 'dormant actors retained');
  assert.equal(saved.body.config.tiers.core.max_tokens, 4000);
  assert.equal(saved.body.config.execution_mode, 'independent');

  // Runtime polling must not replace an in-progress configuration draft.
  scene = { initialized: true, kind: 'free', turn: 3, stalled: true, stalled_turns: 3, actors: ['A', 'B', 'C', 'D', 'E'], last_outcomes: [{ actor_id: 'A', action: 'wait', accepted: true }] };
  await page.getByRole('button', { name: '刷新运行状态', exact: true }).click();
  await page.getByText('已暂停重复生成', { exact: false }).waitFor();
  await page.getByText('添加可追溯的环境条件', { exact: true }).click();
  await page.getByLabel('角色能观察到的条件', { exact: false }).fill('公开计时器亮起');
  await page.getByLabel('作者理由', { exact: false }).fill('基于当前停滞事件提供新线索');
  await page.getByRole('button', { name: '确认环境条件', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('textarea[placeholder^="描述环境变化"]')?.value === '');
  const pressure = writes.find(w => w.path === '/api/world/agents/pressures');
  assert.equal(pressure.world, world);
  assert.equal(pressure.body.source_id, 'event:4:test');
  assert.deepEqual(pressure.body.actors, ['A', 'B', 'C', 'D', 'E']);

  conflict = true;
  await card('A').getByRole('combobox').selectOption('background');
  await page.getByRole('button', { name: '刷新运行状态', exact: true }).click();
  assert.equal(await card('A').getByRole('combobox').inputValue(), 'background');
  await page.getByRole('button', { name: '保存三档与全部人物设置' }).click();
  await page.getByRole('alert').filter({ hasText: '草稿已保留' }).waitFor();
  assert.equal(await card('A').getByRole('combobox').inputValue(), 'background');
  conflict = false;

  await page.getByLabel('API Key', { exact: false }).fill('TEST-ONLY-PRIVATE-CANARY');
  await page.getByRole('button', { name: '保存服务', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('input[type=password][autocomplete=new-password]')?.value === '');
  assert.equal(writes.find(w => w.path === '/api/llm/providers').body.providers.a.api_key, 'TEST-ONLY-PRIVATE-CANARY');
  const storage = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }));
  assert.ok(!storage.includes('PRIVATE-CANARY'));
  assert.equal(tests.length, 0, 'saving does not test connection');
  await page.getByRole('button', { name: '测试已保存配置（一次请求）' }).click();
  await page.getByText('连接成功', { exact: false }).waitFor();
  assert.equal(tests.length, 1);

  await page.getByTitle('选择世界').selectOption('另一个世界');
  await page.getByRole('heading', { name: '世界人物模型 · 另一个世界' }).waitFor();
  await page.getByText('共 5 个 Agent', { exact: false }).waitFor();
  assert.equal(await card('A').getByRole('combobox').inputValue(), 'core', 'old draft cleared on world change');
  await page.getByRole('button', { name: '保存三档与全部人物设置' }).click();
  await page.getByText('配置版本 1', { exact: false }).waitFor();
  assert.equal(writes.filter(w => w.path === '/api/world/agents/config').at(-1).world, '另一个世界');
  assert.deepEqual(errors, []);
  console.log('PASS: five-character configuration, model override, dormant preservation, conflict drafts, credential cleanup, explicit test and world switching. All API requests mocked.');
} finally {
  await browser.close();
  await new Promise(resolve => server.close(resolve));
}
