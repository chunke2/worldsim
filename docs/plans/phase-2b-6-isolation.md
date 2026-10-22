# 2B-6：五角色实际请求边界验收

测试：`TestRouterFivePrivateCanariesAtActualHTTPBoundary`。

临时世界注册五名角色，每人单独写入不同的自身资料 canary。创建两个本机 HTTP mock Provider，给一名角色设置强模型覆盖，其余继承便宜模型档位。每个 CharacterAgent 通过正式 ModelRouter 分别请求服务。

在服务端解析实际 HTTP body，验证：每次恰好一条 system 与一条 user 消息，只含该人物尚未公开的私有 canary；其余四人的私有 canary 均不存在；认证、模型和 max_tokens 精确匹配；五个人均有独立请求和审计。

mock 响应故意携带 reasoning_content；检查认知与审计文件不存在此原始 reasoning 或任何 Provider 密钥。返回的 Token 使用量正确记录。该验收不依赖只检查文件目录或请求前的中间结构。

通过 `go test ./...` 与相关包 race 检测。只有本机 mock 请求，无真实 API Token。合法信息传播后角色当然可以知道他人公开说出的内容；canary 测试针对尚未传播的私有信息，不禁止正常沟通。

这验证请求/上下文和存储的逻辑隔离，不是进程级 OS 沙箱。模型内容是否完全遵循人设、能否写出好小说，仍需后续系统与文学质量验收。
