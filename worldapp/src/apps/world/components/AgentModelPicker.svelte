<script>
  export let providers = {};
  export let providerID = '';
  export let model = '';
  export let prefix;
  export let disabled = false;
</script>

<div class="flex flex-wrap gap-2">
  <label class="text-xs flex-1 min-w-32">API 服务
    <select class="select select-sm select-bordered w-full" bind:value={providerID} {disabled}>
      <option value="">请选择</option>
      {#if providerID && !providers[providerID]}<option value={providerID}>{providerID}（已失效）</option>{/if}
      {#each Object.entries(providers) as [id, p]}<option value={id}>{p.name || id} · {id}</option>{/each}
    </select>
  </label>
  <label class="text-xs flex-1 min-w-32">模型名称
    <input class="input input-sm input-bordered w-full" list={prefix + '-models'} bind:value={model} {disabled} placeholder="填写服务实际支持的模型名" />
    <datalist id={prefix + '-models'}>{#each providers[providerID]?.models || [] as m}<option value={m}></option>{/each}</datalist>
  </label>
</div>
