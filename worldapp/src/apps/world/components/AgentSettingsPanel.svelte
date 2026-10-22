<script>
  import { onMount, onDestroy } from 'svelte';
  import { currentWorld, worldState, loopInfo, toast } from '../lib/stores.js';
  import { request, jsonOptions, providerPatch, resolvedModel, agentSaveBody } from '../lib/agentSettings.js';
  import AgentModelPicker from './AgentModelPicker.svelte';
  import AgentRuntimePanel from './AgentRuntimePanel.svelte';

  const tiers = [['core', '核心人物'], ['supporting', '重要配角'], ['background', '背景人物']];
  const world = $currentWorld; // Parent keys this panel by world; drafts cannot cross worlds.
  let alive = true;
  let providerView = null, config = null, registryRevision = 0, agents = [];
  let loading = true, busy = false, error = '', conflict = false, validation = '';
  let configSnapshot = '', providerSnapshot = JSON.stringify(blankForm());
  let selected = '', form = blankForm(), testModel = '', testResult = '';
  $: providers = providerView?.providers || {};
  $: dirty = config && JSON.stringify(config) !== configSnapshot;
  $: providerDirty = JSON.stringify(form) !== providerSnapshot;
  $: worldRunning = !!$loopInfo?.running;
  $: legacy = selected === 'legacy-default';

  function blankForm() {
    return { id: '', name: '', base_url: '', api_key: '', clear_api_key: false, models: '', timeout_seconds: 300, max_parallel: 1, url_strict: false, extra_body: '' };
  }
  function showError(e) {
    if (!alive) return;
    conflict = e.status === 409;
    error = e.message + (conflict ? '。可能有运行任务或配置已变更；草稿已保留，请停止任务或重新读取后再保存。' : '');
  }
  function chooseProvider(id, force = false) {
    if (!force && providerDirty && !window.confirm('放弃当前 API 服务的未保存修改？')) return;
    selected = id;
    const p = providerView?.providers?.[id];
    form = p ? { ...blankForm(), ...p, id, models: (p.models || []).join('\n'), api_key: '', extra_body: '' } : blankForm();
    // Do not echo public helper properties into an update.
    providerSnapshot = JSON.stringify(form);
    testModel = p?.models?.[0] || '';
    testResult = '';
  }
  async function load(confirmDraft = true) {
    if (confirmDraft && (dirty || providerDirty) && !window.confirm('重新读取会放弃未保存修改，继续？')) return;
    loading = true; error = ''; conflict = false;
    try {
      const p = await request('/api/llm/providers');
      if (!alive) return;
      providerView = p;
      chooseProvider(p.providers[selected] ? selected : Object.keys(p.providers)[0] || '', true);
      if (world) {
        const [c, a] = await Promise.all([
          request('/api/world/agents/config', { world }), request('/api/world/agents', { world }),
        ]);
        if (!alive) return;
        if (c.registry_revision !== a.registry_revision || c.config.revision !== a.config_revision) throw new Error('读取期间人物配置发生变化，请重新读取');
        config = c.config; registryRevision = c.registry_revision; agents = a.agents;
        validation = c.validation_error || '';
        configSnapshot = JSON.stringify(config);
      }
    } catch (e) { showError(e); }
    finally { if (alive) loading = false; }
  }
  async function action(fn) {
    busy = true; error = ''; conflict = false;
    try { await fn(); } catch (e) { showError(e); }
    finally { if (alive) busy = false; }
  }
  async function updateProviders(body, savedID = selected) {
    const saved = await request('/api/llm/providers', jsonOptions('PUT', { revision: providerView.revision, ...body }));
    if (!alive) return;
    providerView = saved;
    chooseProvider(savedID, true);
    toast('API 服务设置已保存', 'success');
  }
  function saveProvider() {
    action(async () => {
      const id = form.id.trim();
      if (!/^[A-Za-z0-9_-]{1,128}$/.test(id)) throw new Error('服务 ID 只能包含英文字母、数字、下划线和短横线');
      if (!selected && providers[id]) throw new Error('该服务 ID 已存在，请选择后编辑');
      await updateProviders({ providers: { [id]: providerPatch(form, legacy) } }, id);
    });
  }
  function deleteProvider() {
    if (!window.confirm('删除这个 API 服务？已被人物或档位引用的服务将被拒绝删除。')) return;
    action(() => updateProviders({ delete: [selected] }, ''));
  }
  function testProvider() {
    if (!window.confirm('使用已保存的服务配置发送一次短模型请求，可能产生少量费用。继续？')) return;
    action(async () => {
      const result = await request(`/api/llm/providers/${encodeURIComponent(selected)}/test`, jsonOptions('POST', { model: testModel.trim() }));
      if (alive) testResult = `连接成功 · ${result.model} · ${result.latency_ms} ms`;
    });
  }
  function saveAgents() {
    action(async () => {
      if ($currentWorld !== world) throw new Error('世界已切换，请重新读取');
      const result = await request('/api/world/agents/config', { world, ...jsonOptions('PUT', agentSaveBody(config, registryRevision)) });
      if (!alive) return;
      config = result.config; registryRevision = result.registry_revision;
      configSnapshot = JSON.stringify(config); validation = '';
      toast('人物模型与运行方式已保存', 'success');
    });
  }
  function reconcile() {
    if ((dirty || providerDirty) && !window.confirm('同步人物后将重新读取，放弃未保存设置？')) return;
    action(async () => {
      await request('/api/world/agents/reconcile', { world, ...jsonOptions('POST', {}) });
      await load(false);
    });
  }
  function toggleOverride(id, checked) {
    if (checked) {
      const inherited = resolvedModel(config, id);
      config.characters[id].model_override = { provider_id: inherited.provider_id, model: inherited.model };
    } else delete config.characters[id].model_override;
    config = config;
  }
  function initializeCards() {
    if (!window.confirm('为当前世界同一房间的五名活跃人物初始化死亡牌局？这会写入游戏规则和各自手牌，不调用模型，不能覆盖已初始化的场景。')) return;
    action(async () => {
      await request('/api/world/agents/scenario', { world, ...jsonOptions('POST', { kind: 'liars_room', lives: 1 }) });
      await load(false);
      toast('五人牌局已初始化；点击操作台推进回合才会调用角色模型', 'success');
    });
  }
  onMount(() => { load(false); });
  onDestroy(() => { alive = false; form.api_key = ''; });
</script>

<div class="space-y-5">
  {#if world}<AgentRuntimePanel {world} />{/if}
  <div class="flex flex-wrap items-center gap-3">
    <h2 class="text-lg font-semibold">角色 Agent 与模型服务</h2>
    <button class="btn btn-sm" disabled={busy || loading} on:click={() => load()}>重新读取</button>
    {#if dirty || providerDirty}<span class="badge badge-warning">有未保存修改</span>{/if}
  </div>
  <p class="text-sm">选择“独立角色运行”后，推进世界会唤醒相关人物，各自使用自己的上下文与指定模型。旧世界默认保持兼容模式。每次只运行需要行动的人物；本页读取和保存不会自动调用模型。</p>
  {#if error}<div role="alert" class="alert {conflict ? 'alert-warning' : 'alert-error'} whitespace-pre-wrap">{error}</div>{/if}
  {#if loading}<p role="status">正在读取配置…</p>{:else}
    <section class="paper rounded-xl p-4 space-y-3">
      <h3 class="font-semibold">API 服务（所有世界共用）</h3>
      <p class="text-xs">支持多个 OpenAI 兼容服务。密钥只保存在本机服务端；留空保留原密钥。模型目录留空允许手填，填写后仅允许目录中的模型。</p>
      {#if providerView}
        <div class="flex flex-wrap gap-2">
          {#each Object.entries(providers) as [id, p]}
            <button class="btn btn-sm {selected === id ? 'btn-primary' : ''}" disabled={busy} on:click={() => chooseProvider(id)}>{p.name || id}{providerView.default_provider === id ? ' · 默认' : ''}</button>
          {/each}
          <button class="btn btn-sm" disabled={busy} on:click={() => chooseProvider('')}>＋ 新服务</button>
        </div>
        <fieldset class="space-y-3" disabled={busy}>
          <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
            <label class="text-xs">服务 ID<input class="input input-sm input-bordered w-full" bind:value={form.id} disabled={!!selected} placeholder="例如 deepseek" /></label>
            <label class="text-xs">显示名称<input class="input input-sm input-bordered w-full" bind:value={form.name} disabled={legacy} /></label>
            <label class="text-xs md:col-span-2">Base URL<input class="input input-sm input-bordered w-full" bind:value={form.base_url} placeholder="https://服务地址/v1" /></label>
            <label class="text-xs">API Key{providers[selected]?.has_api_key ? '（已保存）' : '（未保存）'}<input type="password" autocomplete="new-password" class="input input-sm input-bordered w-full" bind:value={form.api_key} placeholder="留空保留原密钥" /></label>
            <label class="flex items-center gap-2 text-xs"><input type="checkbox" class="checkbox checkbox-sm" bind:checked={form.clear_api_key} />明确清除已保存密钥</label>
            <label class="text-xs">超时（秒；0 使用默认）<input type="number" min="0" step="1" class="input input-sm input-bordered w-full" bind:value={form.timeout_seconds} /></label>
            <label class="text-xs">服务并发上限（0 使用默认）<input type="number" min="0" step="1" class="input input-sm input-bordered w-full" bind:value={form.max_parallel} disabled={legacy} /></label>
            <label class="text-xs md:col-span-2">可用模型目录（每行一个，或逗号分隔）<textarea class="textarea textarea-bordered w-full" rows="2" bind:value={form.models} disabled={legacy}></textarea></label>
          </div>
          <label class="flex gap-2 items-center text-xs"><input type="checkbox" class="checkbox checkbox-sm" bind:checked={form.url_strict} />保留地址中的 API 路径，仅补 /chat/completions（不自动插入 /v1）</label>
          <details><summary class="text-xs cursor-pointer">高级请求参数</summary>
            <label class="text-xs">extra_body（JSON 对象；留空保留，填写新对象替换）<textarea class="textarea textarea-bordered w-full font-mono" rows="2" bind:value={form.extra_body} placeholder={'例如 {"enable_thinking":false}'}></textarea></label>
          </details>
          {#if legacy}<p class="text-xs">legacy-default 来自原有模拟管线，名称、模型目录和并发字段由兼容配置管理，不能删除。</p>{/if}
          <div class="flex flex-wrap gap-2">
            <button class="btn btn-primary btn-sm" on:click={saveProvider}>保存服务</button>
            {#if selected}
              <button class="btn btn-sm" disabled={providerDirty || providerView.default_provider === selected} on:click={() => action(() => updateProviders({ default_provider: selected }))}>设为新配置默认服务</button>
              <button class="btn btn-error btn-outline btn-sm" disabled={legacy} on:click={deleteProvider}>删除服务</button>
            {/if}
          </div>
        </fieldset>
        {#if selected}
          <div class="flex flex-wrap gap-2 items-end">
            <label class="text-xs flex-1">连接测试模型<input class="input input-sm input-bordered w-full" bind:value={testModel} /></label>
            <button class="btn btn-sm" disabled={busy || providerDirty || !testModel.trim()} on:click={testProvider}>测试已保存配置（一次请求）</button>
          </div>
          <p class="text-xs" role="status">{testResult || '不会自动测试连接或请求模型。修改服务后请先保存。'}</p>
        {/if}
      {/if}
    </section>

    <section class="paper rounded-xl p-4 space-y-4">
      <h3 class="font-semibold">世界人物模型 · {world || '尚未选择世界'}</h3>
      {#if world && config}
        <p class="text-xs">共 {agents.length} 个 Agent，活跃 {agents.filter(a => a.state === 'active').length} 个，休眠 {agents.filter(a => a.state !== 'active').length} 个。配置版本 {config.revision}；注册版本 {registryRevision}。模型设置不会改变人物的生死或出场状态。</p>
        {#if validation}<p class="text-sm text-warning">当前配置诊断：{validation}。修正服务和三档模型后保存。</p>{/if}
        {#if worldRunning}<p class="text-warning text-sm">后台模拟运行中；请停止并等待当前轮结束后保存人物设置。</p>{/if}
        <fieldset class="space-y-4" disabled={busy || worldRunning}>
          <label class="text-sm">运行方式
            <select class="select select-sm select-bordered w-full" bind:value={config.execution_mode}>
              <option value="">旧版本兼容（默认）</option><option value="legacy">旧版本兼容</option><option value="independent">独立角色运行</option>
            </select>
          </label>
          <p class="text-xs">独立模式使用下方人物模型；启用后再点击“推进”才产生请求。兼容管线的 off/mock 不会被当作角色真实模型。反思周期目前作为策略记录，不额外自动发起反思请求。</p>
          {#if config.execution_mode === 'independent'}
            <button class="btn btn-sm btn-outline" disabled={dirty || providerDirty || agents.filter(a => a.state === 'active').length !== 5} on:click={initializeCards}>初始化五人死亡牌局（零模型请求）</button>
          {/if}
          {#each tiers as [tier, label]}
            <div class="border border-base-content/10 rounded-lg p-3 space-y-2">
              <h4 class="text-sm font-semibold">{label}</h4>
              <AgentModelPicker {providers} prefix={'tier-' + tier} bind:providerID={config.tiers[tier].provider_id} bind:model={config.tiers[tier].model} disabled={busy || worldRunning} />
              <details><summary class="text-xs cursor-pointer">独立运行预算与唤醒策略</summary>
                <div class="grid grid-cols-2 gap-2 mt-2">
                  {#each [['max_tokens', '输出 Token 上限'], ['context_budget_tokens', '上下文 Token 预算'], ['max_parallel', '档位并发上限'], ['reflection_every', '每几轮反思']] as [key, title]}
                    <label class="text-xs">{title}（0 使用默认）<input type="number" min="0" step="1" class="input input-sm input-bordered w-full" bind:value={config.tiers[tier][key]} /></label>
                  {/each}
                  <label class="text-xs">温度（留空使用默认）<input type="number" min="0" max="2" step="0.1" class="input input-sm input-bordered w-full" bind:value={config.tiers[tier].temperature} /></label>
                  <label class="text-xs">唤醒时机<select class="select select-sm select-bordered w-full" bind:value={config.tiers[tier].wake_policy}><option value="">默认</option><option value="every_relevant_turn">相关回合均唤醒</option><option value="on_involvement">参与当前场景时唤醒</option><option value="on_direct_event">收到外部观察时唤醒</option></select></label>
                </div>
              </details>
            </div>
          {/each}
          <h4 class="text-sm font-semibold">逐人物设置（包含休眠人物）</h4>
          {#each Object.entries(config.characters) as [id, character]}
            <div class="border border-base-content/10 rounded-lg p-3 space-y-2">
              <div class="flex flex-wrap gap-2 items-center">
                <strong class="text-sm">{$worldState?.entities?.[id]?.name || id}</strong><span class="text-xs">{id} · {agents.find(a => a.character_id === id)?.state || '未注册'}</span>
                <label class="text-xs">重要性<select class="select select-sm select-bordered ml-2" bind:value={character.importance}>{#each tiers as [value, label]}<option {value}>{label}</option>{/each}</select></label>
              </div>
              <label class="text-xs flex items-center gap-2"><input type="checkbox" class="checkbox checkbox-sm" checked={!!character.model_override} on:change={e => toggleOverride(id, e.target.checked)} />为此人物单独指定模型</label>
              {#if character.model_override}
                <AgentModelPicker {providers} prefix={'character-' + id} bind:providerID={character.model_override.provider_id} bind:model={character.model_override.model} disabled={busy || worldRunning} />
              {:else}<p class="text-xs">跟随档位：{resolvedModel(config, id).provider_id || '未选择服务'} / {resolvedModel(config, id).model || '未填写模型'}</p>{/if}
            </div>
          {/each}
          {#if !agents.length}<p class="text-sm">当前没有已注册人物。先初始化世界，或使用“同步世界人物”；同步不调用模型。</p>{/if}
          <div class="flex gap-2">
            <button class="btn btn-primary btn-sm" disabled={!dirty && config.revision > 0} on:click={saveAgents}>保存三档与全部人物设置</button>
            <button class="btn btn-sm" on:click={reconcile}>同步世界人物</button>
          </div>
        </fieldset>
      {:else}<p class="text-sm">请先选择世界并读取人物配置；API 服务可独立配置。</p>{/if}
    </section>
  {/if}
</div>
