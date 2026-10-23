<script>
  export let providers = {};
  export let providerID = '';
  export let model = '';
  export let disabled = false;
  $: modelOptions = providers[providerID]?.models || [];
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
    {#if modelOptions.length}
      <select class="select select-sm select-bordered w-full" bind:value={model} {disabled}>
        {#if model && !modelOptions.includes(model)}<option value={model}>{model}（目录外旧值）</option>{/if}
        {#each modelOptions as option}<option value={option}>{option}</option>{/each}
      </select>
    {:else}
      <input class="input input-sm input-bordered w-full" bind:value={model} {disabled} placeholder="服务未提供目录，请填写模型名" />
    {/if}
  </label>
</div>
