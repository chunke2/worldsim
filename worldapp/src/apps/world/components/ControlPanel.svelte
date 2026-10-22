<script>
  import { onMount } from 'svelte';
  import { j } from '../lib/api.js';
  import { heroName, currentWorld, refreshAll, toast, lastResult, activeTab, loopInfo } from '../lib/stores.js';
  import PauseModal from './PauseModal.svelte';

  let pauseModal;

  let hero = '';
  let llmMode = 'real';
  let baseUrl = '';
  let modelName = '';
  let apiKey = '';
  let tierFast = '', tierNormal = '', tierPremium = '';
  let apiRevision = '', hasKey = false, configLoaded = false;
  async function loadLegacyConfig() {
    const d = await j('/api/novel/config/api');
    if (!d.revision) { toast('无法读取已保存模型配置，请重试', 'error'); return; }
    baseUrl = d.base_url || ''; modelName = d.model || '';
    tierFast = d.model_tiers?.fast || modelName;
    tierNormal = d.model_tiers?.normal || modelName;
    tierPremium = d.model_tiers?.premium || modelName;
    apiRevision = d.revision; hasKey = !!d.has_api_key; configLoaded = true;
    apiKey = '';
  }
  onMount(() => { loadLegacyConfig(); });
  $: if ($loopInfo) applyLoop($loopInfo);
  let daysNum = 1;
  let simMode = 'auto';
  let loopDays = 1000;
  let loopMode = 'auto';
  let running = false;
  let loopRunning = false;
  let loopProgress = 0;
  let loopDetail = '循环未启动';

  async function initWorld() {
    const name = hero.trim();
    const d = await j('/api/world/init', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ protagonist: name })
    });
    if (d.ok) {
      toast('世界初始化完成 · 主角：' + (name || '（世界书自拟）'), 'success');
      heroName.set(name);
      refreshAll();
    } else {
      toast('失败：' + (d.error || ''), 'error');
    }
  }

  async function setLLM() {
    const body = { mode: llmMode };
    if (llmMode === 'real') {
      body.revision = apiRevision;
      body.base_url = baseUrl.trim();
      body.model = modelName.trim();
      body.api_key = apiKey.trim();
      body.model_tiers = { fast: tierFast, normal: tierNormal, premium: tierPremium };
    }
    const d = await j('/api/world/sim/llm', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    });
    if (d.ok) {
      apiKey = '';
      toast('兼容管线 LLM 模式：' + llmMode, 'success');
      if (llmMode === 'real') await loadLegacyConfig();
    }
    else toast('失败：' + (d.error || ''), 'error');
  }

  async function runDays() {
    const days = Math.min(parseInt(daysNum) || 1, 30);
    running = true;
    const d = await j('/api/world/sim/day', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ days, mode: simMode })
    });
    running = false;
    if (!d.ok) { toast('失败：' + (d.error || ''), 'error'); return; }
    lastResult.set(d);
    toast('模拟完成：跑至 Day ' + d.day, 'success');
    refreshAll();
    if (d.paused) pauseModal.show(d);
  }

  async function startLoop() {
    const d = await j('/api/world/loop', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'start', days: parseInt(loopDays) || 1000, mode: loopMode })
    });
    if (d.ok) toast('循环启动 → 目标 Day ' + d.target_day, 'success');
    else toast('失败：' + (d.error || ''), 'error');
    fetchLoop();
  }

  async function stopLoop() {
    await j('/api/world/loop', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'stop' })
    });
    toast('已停止循环');
    fetchLoop();
  }

  async function fetchLoop() {
    const d = await j('/api/world/loop');
    if (!d || d.running === undefined) return;
    loopInfo.set(d);
  }

  function applyLoop(d) {
    loopRunning = d.running;
    if (d.running) {
      loopProgress = Math.min(100, Math.round((d.day / (d.target_day || 1)) * 100));
      loopDetail = `运行中 · ${d.world || ''} · Day ${d.day} → 目标 ${d.target_day}${d.ready ? ' · 🎬 已就绪' : ''}`;
    } else {
      if (d.target_day > 0 && d.day > 0) {
        loopDetail = `已停止 · Day ${d.day}${d.ready ? ' · 🎬 素材就绪，可以写小说了！' : ' · 未就绪'}`;
      } else {
        loopDetail = '循环未启动';
      }
    }
  }
</script>

<div class="paper rounded-xl p-4 space-y-3">
  <div class="flex items-center gap-2">
    <span class="text-lg cloud-icon">⚙</span>
    <h2 class="text-sm font-semibold text-primary tracking-widest">操作台</h2>
    <div class="flex-1"></div>
    <span class="badge badge-outline badge-xs">{ $currentWorld || '未选择世界' }</span>
  </div>

  <button class="btn btn-sm btn-outline w-full" on:click={() => activeTab.set('agents')}>API 服务 / 角色 Agent 设置</button>
  <p class="text-[11px] text-base-content/60">三档人物模型在上方设置；下方为当前模拟使用的兼容管线。</p>

  <div>
    <label class="label py-0 text-xs text-base-content/60">主角名</label>
    <div class="flex gap-2">
      <input class="input input-sm input-bordered flex-1" placeholder="主角名" bind:value={hero} />
      <button class="btn btn-primary btn-sm" on:click={initWorld}>初始化</button>
    </div>
  </div>

  <div>
    <label class="label py-0 text-xs text-base-content/60">LLM 模式</label>
    <div class="flex gap-2">
      <select class="select select-sm select-bordered flex-1" bind:value={llmMode}>
        <option value="real">real · 真实LLM</option>
        <option value="mock">mock · 本地模拟</option>
        <option value="off">off · 模板</option>
      </select>
      <button class="btn btn-ghost btn-sm" on:click={setLLM} disabled={running || loopRunning || (llmMode === 'real' && !configLoaded)}>应用</button>
    </div>
  </div>

  {#if llmMode === 'real'}
  <div class="space-y-1.5">
    <input class="input input-sm input-bordered w-full" placeholder="Base URL" bind:value={baseUrl} />
    <input class="input input-sm input-bordered w-full" placeholder="模型" bind:value={modelName} />
    <input class="input input-sm input-bordered w-full" placeholder={hasKey ? 'API Key 已保存；留空保留' : 'API Key'} type="password" bind:value={apiKey} />
    <button class="btn btn-ghost btn-xs" on:click={loadLegacyConfig}>重新读取已保存配置</button>
    <div class="text-xs text-base-content/50 pt-1">🧠 模型分层（各司其职）</div>
    <div class="flex items-center gap-1.5">
      <span class="text-xs text-base-content/50 w-14 shrink-0">⚡ 高频</span>
      <input aria-label="兼容管线高频模型" class="input input-xs input-bordered flex-1 min-w-0" bind:value={tierFast} />
    </div>
    <div class="flex items-center gap-1.5">
      <span class="text-xs text-base-content/50 w-14 shrink-0">🔷 推理</span>
      <input aria-label="兼容管线推理模型" class="input input-xs input-bordered flex-1 min-w-0" bind:value={tierNormal} />
    </div>
    <div class="flex items-center gap-1.5">
      <span class="text-xs text-base-content/50 w-14 shrink-0">💎 产出</span>
      <input aria-label="兼容管线产出模型" class="input input-xs input-bordered flex-1 min-w-0" bind:value={tierPremium} />
    </div>
    <div class="text-[10px] text-base-content/40">⚡事件/NPC · 🔷主角/GM/反思 · 💎小说/抉择</div>
  </div>
  {/if}

  <div>
    <label class="label py-0 text-xs text-base-content/60">单次模拟</label>
    <div class="flex gap-2">
      <input class="input input-sm input-bordered w-16" type="number" min="1" max="30" bind:value={daysNum} />
      <select class="select select-sm select-bordered flex-1" bind:value={simMode}>
        <option value="auto">⚙️ 自动</option>
        <option value="scene">🎬 Scene</option>
        <option value="summary">📝 Summary</option>
        <option value="skip">⏩ Skip</option>
      </select>
      <button class="btn btn-primary btn-sm" on:click={runDays} disabled={running}>
        {#if running}<span class="loading loading-spinner loading-xs"></span>{:else}▶ 跑{/if}
      </button>
    </div>
  </div>

  <div class="scroll-rule"></div>

  <div>
    <label class="label py-0 text-xs text-base-content/60">🔁 后台持续运行（到就绪自动停）</label>
    <div class="flex gap-2">
      <input class="input input-sm input-bordered w-20" type="number" min="1" bind:value={loopDays} />
      <select class="select select-sm select-bordered flex-1" bind:value={loopMode}>
        <option value="auto">⚙️ 自动</option>
        <option value="scene">🎬 Scene</option>
        <option value="summary">📝 Summary</option>
      </select>
      {#if !loopRunning}
        <button class="btn btn-primary btn-sm" on:click={startLoop}>▶ 开始</button>
      {:else}
        <button class="btn btn-error btn-sm" on:click={stopLoop}>⏹ 停止</button>
      {/if}
    </div>
    <progress class="progress progress-primary w-full h-1.5 mt-2" value={loopProgress} max="100"></progress>
    <div class="text-[11px] text-base-content/50 mt-1">{loopDetail}</div>
  </div>
</div>

<PauseModal bind:this={pauseModal} />
