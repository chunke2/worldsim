// These helpers never persist credentials in browser storage.
export async function request(url, { world, ...options } = {}) {
  if (world) url += `${url.includes('?') ? '&' : '?'}world=${encodeURIComponent(world)}`;
  const response = await fetch(url, options);
  const data = await response.json();
  if (!response.ok || data.ok === false) {
    const error = new Error(data.error || `请求失败 (${response.status})`);
    error.status = response.status;
    throw error;
  }
  return data;
}

export function jsonOptions(method, body) {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) };
}

export function providerPatch(form, legacy = false) {
  const patch = {
    base_url: form.base_url.trim(), timeout_seconds: Number(form.timeout_seconds) || 0,
    url_strict: !!form.url_strict,
  };
  if (!legacy) Object.assign(patch, {
    name: form.name.trim(), protocol: 'openai_compatible',
    models: [...new Set(form.models.split(/[\n,]/).map(m => m.trim()).filter(Boolean))],
    max_parallel: Number(form.max_parallel) || 0,
  });
  if (form.api_key.trim()) patch.api_key = form.api_key.trim();
  if (form.clear_api_key) {
    if (patch.api_key) throw new Error('请在填写新密钥和清除旧密钥之间选择一项');
    patch.clear_api_key = true;
  }
  if (form.extra_body.trim()) {
    const extra = JSON.parse(form.extra_body);
    if (!extra || typeof extra !== 'object' || Array.isArray(extra)) throw new Error('extra_body 必须是 JSON 对象');
    patch.extra_body = extra;
  }
  return patch;
}

export function resolvedModel(config, id) {
  const character = config.characters[id];
  return character.model_override || config.tiers[character.importance];
}

export function agentSaveBody(config, registryRevision) {
  // Clone the full document: preserve unedited policies, dormant actors and CAS revision.
  return { registry_revision: registryRevision, config: JSON.parse(JSON.stringify(config)) };
}
