# 2B-2：旧记忆可逆迁移

## 修改前判断

1. 已确认问题：旧 `agents_memory.json` 混合多角色近期记忆、摘要和旧推断，不能直接当作可信观察灌入新认知。
2. 优先级：P1 角色认知迁移，P0 原文保留、失败恢复与身份边界。
3. 验证收益：源文件字节不变，完整快照可核对；身份匹配的记录进入单角色隔离归档，冲突不猜测；迁移重试不重复、不覆盖。
4. 最小证明：临时五人源文件、真实注册表、预览/应用/重试/冲突/写失败测试；认知文件始终不变，无 LLM 调用。

## 决策

- 仅显式调用迁移；CLI 默认为预览，应用必须提供预览生成的计划 ID。
- 同时支持 `{memories:{人物:[]}, archive:{人物:[]}}` 与直接 `{人物:[]}` 两种格式。拒绝重复 JSON 键和结构损坏，避免静默遗漏或覆盖。未知记录字段保存在原始记录与完整备份中。
- 精确比较注册人物 ID、分组键和记录 actor，不做模糊匹配。空 actor、未知人物、冲突或异常条目进入全局待处理区，不能借迁移获得他人知识。
- 原始快照和待处理区在 `memory_migrations/{planID}/`，人物隔离副本在 `agents/{AgentID}/legacy/{planID}.json`。全部标记为 quarantined/unverified，不写入 cognition.json。
- 先备份、再写隔离副本、最后写提交清单。隔离读取必须存在完整清单并验证文件摘要；中途失败留下不可激活副本，原计划可安全重试。已有同名文件仅允许字节相同，绝不覆盖。
- 源文件和注册表摘要共同确定计划，提交前再次检查；源文件变化后旧计划拒绝应用。
- 可逆性来自保留源文件与独立隔离输出：忽略迁移归档即可继续使用原记忆。没有删除、回灌或自动替换当前记忆的操作。

## 边界

本阶段不会判断旧事件是否真的对该人物可见，即使原记录带 source_events 也不自动授信。迁移内容不会进入当前认知、模型上下文或世界事实。后续验证来源后才可单独转为认知观察。

迁移工具应在世界停止时运行。目录锁和活动门禁仅在同进程有效，不支持其他进程同时写同一世界；摘要检查可识别已观察到的变化，不替代跨进程事务锁。不会自动对用户现有世界运行迁移。

## 使用与验收

### 输出布局

```text
world/
  agents_memory.json                         # 不修改、不移动
  agents/{AgentID}/
    cognition.json                           # 不修改或初始化
    legacy/{planID}.json                      # 仅该角色、待验证的旧记录
  memory_migrations/{planID}/
    source.json                              # 输入文件逐字节备份
    registry.json                            # 身份注册表快照
    unassigned.json                          # 身份不明/冲突/异常原始条目
    manifest.json                            # 最后提交；元数据、计数及文件摘要
```

每个角色隔离归档包含原记录、原分区（memories/archive）、原索引、世界/角色/Agent 身份、源文件摘要，以及 `quarantined` 状态。历史摘要、旧计划和反思同样不自动授信。source_events、credibility 和未知字段在原记录中保留，不能仅凭它们提升为可信事实。

缺少 actor、actor 与分组冲突、未知人物、空内容、非对象记录、未知 kind 或显式 reasoning/prompt/author_plan/api_key 字段进入待处理区，原文仍在备份和待处理记录中。结构损坏、重复 JSON 键、未知顶层字段直接拒绝整个迁移；不做静默修复。

### 接口和命令

```text
PreviewLegacyMigration(worldDir)             -> 只读计划；不含记忆正文
ApplyLegacyMigration(worldDir, expectedID)   -> 显式应用或幂等重试
Store.ReadLegacyQuarantine(planID)           -> 诊断读取自己的已提交隔离副本
```

隔离读取只加载该角色正文，不读取其他角色或全局待处理区；它不属于 `Read()` 或 `CurrentRecords()`，不会被当作普通认知取出。读取时检查提交清单、当前身份、归档状态和该角色文件摘要。清单缺失、摘要不符或越界计划 ID 均拒绝。清单路径摘要键统一使用 `/`，避免 Windows 路径格式进入持久化协议。

需要已有 `agent_registry.json` 和 `agents_memory.json`；若未注册人物，先通过操作台显式同步人物，再停止对应世界的模拟和其他写入。在仓库根目录执行（示例路径请替换）：

```powershell
# 默认只预览；输出人物计数、异常原因和摘要，不打印记忆正文。
$migrationPreview = go run ./cmd/world-migrate-memory --world 'D:\path\to\world' | ConvertFrom-Json
$migrationPreview.plan

# 检查计数后显式应用；源文件或注册表有变化时旧计划失效。
go run ./cmd/world-migrate-memory --world 'D:\path\to\world' --apply --plan $migrationPreview.plan.id
```

未传 `--apply` 不写文件；`--apply` 没有 `--plan` 将拒绝执行。同一计划重复执行不改变产物。一次迁移的输入与单个产物上限均为 64 MiB，超限停止，不截断内容。已有产物冲突时保留并报错，不能强行覆盖；应先核对完整备份与文件摘要。

### 验证结果

以下全部通过，仅使用临时目录与固定数据：

- `go test ./internal/characteragent ./cmd/world-migrate-memory -count=1`。
- `go test ./...`、`go build ./...`、`go vet ./...`。
- `go test -race ./internal/characteragent ./cmd/world-migrate-memory`。
- 8 项迁移顶层测试：两种输入格式/五人分区/原始字段与摘要保留、无副作用预览、源文件逐字节保留、既有认知不变、重复应用幂等、归属冲突和异常条目隔离、畸形与重复键拒绝、源文件与注册表变化导致计划失效、写失败恢复、提交清单写失败不开放读取、运行门禁、跨世界/路径/篡改拒绝、同进程并发应用收敛。
- 2 项 CLI 测试：默认预览不写、缺少计划拒绝应用、明确应用与重复执行、输出不含私有 canary、非法参数拒绝。

复跑整个 characteragent 包时，2B-1 已有符号链接子测试仍因 Windows 权限明确跳过；本项没有把该保护记为本机已验证通过。

没有读取或迁移用户已有世界、调用真实 API、改动前端或重启服务。新增代码在 `internal/characteragent/legacy_migration.go` 和 `cmd/world-migrate-memory/`，不接入旧模拟循环。

下一项为 2B-3：从已提交事件按角色受众生成局部观察；迁移归档的授信/转换需要明确来源验证，不会因本项完成自动发生。
