# 2B-5：多 Provider 路由、限流与审计

修改前判断：已配置模型引用尚不能执行；P1 模型路由与 P0 凭据/错误边界。验证目标是人物覆盖确实发送到指定本机 mock 服务，服务限流与取消有效，所有失败明确记录，密钥/推理链不进入上下文或审计。只用本机 HTTP mock 验证。

实现不可变 Provider 快照、每 Provider 全局并发闸门、请求超时和显式模型选择，不自动回退。每次调用只有该人物的两条独立消息。自由格式参数不得覆盖 model/messages/stream/max_tokens/temperature 或注入 tools；禁止 HTTP 重定向携带凭据。

审计按角色写入 audit.jsonl，只保存人物、回合、模型引用、输入摘要、耗时、状态、Token 计数与估算标记。远端错误体、响应正文、原始 reasoning、密钥和 API 地址不进入审计或错误消息。审计写失败不能伪装为完整成功。

已实现 ModelRouter、ResolveCharacterModel 和本机 HTTP 测试。Provider 快照不可变，闸门跨 Router 实例共享；取消和超时包含排队阶段。调用不自动重试、回退或访问 /models。已有 HTTP 地址解析语义通过 llm 的公开小包装共享。

审计 `received` 仅表示收到 HTTP 响应，不代表认知已接受；正式认知成功以同文件 ThoughtReceipt 为准，模型协议或来源校验失败追加 cognition_rejected。Token 缺失时保守估算并标记，不声称是实际账单。

3 项 Router 顶层测试（含 2B-6）验证：两 Provider 认证与模型覆盖、限流、取消、受保护 extra_body 拒绝、错误体脱敏和审计失败。全量 Go 测试/build/vet 与相关包 race 检测通过；仅使用本机 httptest。
