<script>
  import { onMount } from 'svelte';
  import { request, jsonOptions } from '../lib/agentSettings.js';
  export let world;
  let status = null, error = '', alive = true, pending = false;
  let detail = '', selected = '', saving = false;
  let kind = 'opportunity', description = '', rationale = '', cost = 1;
  async function refresh() {
    if (!alive || pending || !world) return;
    pending = true;
    try { const result = await request('/api/world/agents/runtime', { world }); if (alive) { status = result; error = ''; } }
    catch (e) { if (alive) error = e.message; }
    finally { pending = false; }
  }
  async function inspect(id, type) {
    try {
      const result = await request(`/api/world/agents/${encodeURIComponent(id)}/${type}`, { world });
      if (alive) { selected = `${id} · ${type}`; detail = JSON.stringify(result[type], null, 2); }
    } catch (e) { if (alive) error = e.message; }
  }
  async function pressure() {
    if (!window.confirm('将这个环境条件写入世界事实日志？不调用模型；角色将自行决定如何回应。')) return;
    saving = true;
    try {
      await request('/api/world/agents/pressures', { world, ...jsonOptions('POST', {
        id: `author-${Date.now()}`, kind, description, rationale,
        due_turn: status.scene.turn + 1, cost: kind === 'opportunity' ? 0 : Number(cost),
        source_id: status.last_event_id,
        actors: status.scene.actors || [],
      }) });
      if (alive) { description = ''; rationale = ''; await refresh(); }
    } catch (e) { if (alive) error = e.message; }
    finally { saving = false; }
  }
  onMount(() => {
    refresh();
    const timer = setInterval(refresh, 3000);
    return () => { alive = false; clearInterval(timer); detail = ''; };
  });
</script>

<section class="border border-base-300 rounded-lg p-4 space-y-3" aria-label="独立角色运行观察">
  <div class="flex gap-3 items-center"><h3 class="font-semibold">独立角色运行观察</h3><button class="btn btn-xs" disabled={pending} on:click={refresh}>刷新运行状态</button></div>
  {#if error}<p role="alert" class="text-error">{error}</p>{/if}
  {#if status?.scene?.initialized}
    <p>场景 {status.scene.kind} · 回合 {status.scene.turn} · 世界版本 {status.world_revision} · 无有效变化 {status.scene.stalled_turns} 回合</p>
    {#if status.scene.next_actor}<p>下一行动者：{status.scene.next_actor} · 桌面牌型 {status.scene.table_rank} · 待质疑 {status.scene.pending_count || 0} 张</p>{/if}
    {#if status.scene.winner}<p>唯一幸存者：{status.scene.winner}</p>{/if}
    {#if status.scene.stalled}<p class="text-warning">已暂停重复生成。可审阅下方环境条件后继续。</p>{/if}
    {#each status.scene.last_outcomes || [] as outcome}
      <p>{outcome.actor_id}：{outcome.action} → {outcome.accepted ? '已裁决执行' : `拒绝：${outcome.reason}`}</p>
    {/each}
  {:else if status}<p>尚未初始化独立场景；此处读取不会运行世界或调用模型。</p>{/if}
  <div class="overflow-x-auto"><table class="table table-sm">
    <thead><tr><th>人物</th><th>状态 / 最近意图</th><th>观察 / 已完成思考</th><th>请求 Token（输入 / 输出）</th><th>作者检查</th></tr></thead>
    <tbody>{#each status?.agents || [] as actor}<tr>
      <td>{actor.character_id}</td><td>{actor.thinking ? '独立请求中' : actor.state} / {actor.last_action || '—'}<br />{actor.last_transport_status || ''}</td>
      <td>{actor.observations} / {actor.thoughts}<br />当前版本新增观察 {actor.latest_observations || 0}</td><td>{actor.prompt_tokens} / {actor.completion_tokens}{actor.usage_estimated ? '（含估算）' : ''}</td>
      <td><button class="btn btn-xs" on:click={() => inspect(actor.character_id, 'cognition')}>私人认知</button> <button class="btn btn-xs" on:click={() => inspect(actor.character_id, 'audit')}>调用审计</button></td>
    </tr>{/each}</tbody>
  </table></div>
  <p class="text-xs opacity-70">received 仅表示接口返回，已完成思考以认知收据为准。Token 来自调用审计，含失败调用已返回的用量；无法获知未返回用量的费用。私人认知仅在作者主动检查时读取。</p>
  {#if Object.keys(status?.diagnostics || {}).length}<pre class="whitespace-pre-wrap text-error">{JSON.stringify(status.diagnostics, null, 2)}</pre>{/if}
  {#if status?.progress?.length}<details><summary>最近回合的剧情推进证据</summary>
    {#each status.progress as turn}<p class="text-sm mt-2">回合 {turn.turn}：{turn.effective_changes?.join('；') || turn.reason}<br /><span class="text-xs opacity-70">{turn.source_id}</span></p>{/each}
  </details>{/if}
  {#if detail}<details open><summary>{selected}（作者私有视图）</summary><button class="btn btn-xs" on:click={() => { detail = ''; }}>关闭</button><pre class="max-h-80 overflow-auto whitespace-pre-wrap text-xs">{detail}</pre></details>{/if}
  {#if status?.scene?.initialized && !status.scene.winner}
    <details><summary>添加可追溯的环境条件</summary>
      <div class="grid gap-2 mt-2">
        <label>类型<select class="select select-bordered w-full" bind:value={kind}><option value="opportunity">可见提示 / 机会</option><option value="deadline">下一回合到期的资源代价</option><option value="resource_cost">下一回合资源消耗</option></select></label>
        <label>角色能观察到的条件<textarea class="textarea textarea-bordered w-full" bind:value={description} maxlength="2000" placeholder="描述环境变化，不能指定角色台词、信念或选择"></textarea></label>
        <label>作者理由（不会进入角色上下文）<input class="input input-bordered w-full" bind:value={rationale} maxlength="2000" /></label>
        {#if kind !== 'opportunity'}<label>每人扣除资源（不足扣至0）<input type="number" min="1" max="1000" class="input input-bordered" bind:value={cost} /></label>{/if}
        <p class="text-xs">依据最近已提交事件 {status.last_event_id}，作用于当前房间所有活跃人物。条件会成为真实世界事件。</p>
        <button class="btn btn-sm" disabled={saving || !description.trim() || !rationale.trim() || !status.last_event_id} on:click={pressure}>确认环境条件</button>
      </div>
    </details>
  {/if}
</section>
