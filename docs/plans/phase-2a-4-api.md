# 2A-4 配置 API

本阶段提供后端配置与运行保护，不代表人物已接入独立决策。操作台在 2A-5 实现。

## 问题与验收

本项解决多 API 和人物模型分配没有持久化接口、旧页面覆盖配置及模拟期间修改配置的问题，属于 2A 的配置基础。可验证质量是：相同 revision 的并发保存只允许一个实际变更成功；世界运行和配置编辑互斥；配置响应不携带密钥；失败写入不发布新版本。

## 接口

| 方法与路径 | 行为 |
|---|---|
| `GET /api/llm/providers` | 返回 Provider 公开视图、默认 Provider 与不透明 revision |
| `PUT /api/llm/providers` | 局部更新、显式删除；必须提交先前读取的 revision |
| `POST /api/llm/providers/{id}/test` | 使用已保存 Key，发一次同步连接测试；可提交 model |
| `GET /api/world/agents` | 返回人物 Agent、活跃状态、有效重要性、解析后的模型引用和错误 |
| `GET /api/world/agents/config` | 返回配置、registry_revision、world_revision、validation_error |
| `PUT /api/world/agents/config` | 保存完整世界 Agent 配置，校验配置和注册表两个版本 |
| `PUT /api/world/agents/{character_id}` | 保存单人物重要性和模型覆盖 |
| `POST /api/world/agents/reconcile` | 明确触发人物注册同步，不调用模型 |

这些接口挂在世界服务，统一入口 `:48092/api/...` 会转发至世界服务。旧小说服务的 API 设置入口仍可使用，并共享文件锁、配置指纹及运行保护。

## Provider 保存示例

```json
{
  "revision": "GET 返回的不透明版本",
  "providers": {
    "secondary": {
      "name": "第二个服务",
      "protocol": "openai_compatible",
      "base_url": "https://example.com/v1",
      "api_key": "仅在本地填写",
      "models": ["model-name"],
      "max_parallel": 2
    }
  },
  "delete": []
}
```

省略/空白密钥保留原值，`clear_api_key=true` 明确清除。更换地址必须同时指定新密钥或清除旧密钥。`legacy-default` 映射到旧顶层设置，不能删除。Provider 不透明版本反映整个 API 配置，因此旧设置入口的变更也能使旧请求失效。

已有世界引用的 Provider 不能删除；模型目录修改后若已有模型引用失效，也拒绝保存。连接测试不返回远端正文、远端错误体或样本文本，避免对方回显凭据。

## 世界及人物保存

整体保存：提交 `{"registry_revision": N, "config": GET中的config}`。`config.revision` 是配置版本，成功保存后加一；注册表版本由人物注册或生命周期变化推进。两者都必须与服务端匹配。

单人物保存：

```json
{
  "revision": 1,
  "registry_revision": 2,
  "character": {
    "importance": "core",
    "model_override": {"provider_id": "secondary", "model": "model-name"}
  }
}
```

省略或将 `model_override` 设为 null 可恢复档位模型。角色重要性保存到 `agent_runtime.json`，优先于注册表最初推断的档位；注册表继续负责稳定身份和生命周期，避免两个文件重复保存用户设置。

新人物自动注册后，GET 在内存中补齐默认人物配置；保存时必须包含所有已注册人物，包括休眠者。GET 不会创建 Provider 配置、注册表或运行配置文件。Provider 未配置时可查看人物，但返回模型配置诊断，不能保存无效引用。

## 范围与运行状态

运行保护是当前单进程中的活动门禁：全局 Provider 编辑排斥所有已接入的运行活动，同一世界运行排斥其人物配置编辑；不同世界可以独立配置。客户端停止循环不等于当前轮已经退出，退出前配置保存仍返回 409。

本阶段接入世界模拟、初始化、手动提案、回档、世界小说生成及旧小说后台任务。独立文字游戏、图像工坊等既有子系统没有在本阶段重构，跨进程写同一目录也尚未支持。

角色认知与审计读取 API 属于 2B；本阶段不伪造这些数据。所有 Agent 列表明确返回 `autonomous_runtime=false`。保存的模型引用将在 2B 的 ModelRouter 接入后用于独立角色请求。
