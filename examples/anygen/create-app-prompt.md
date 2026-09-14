# 新建 AnyGen LLM Proxy App 的构建任务

这是平台契约变化时的辅助构建方案；正常创建请先用 `anygen.py provision`，
它会直接上传本目录 `app/` 中固定源码，不要求 Agent 重新生成 App。

发送前把 `TARGET_CLAW_TOKEN` 替换为已确认的新私人 Agent 的 claw token。
以下范围只授权构建和发布该 App，不授权创建 Key/hook、改 grant 或调用付费模型。
如果用户选择命名槽，须另外明确槽名；默认使用新私人 Agent 的默认槽。

---

请在 `TARGET_CLAW_TOKEN` 的默认 App 槽创建并发布一个私人 LLM Proxy App。
若该槽已有 App，先停止并报告，不要覆盖、删除、fork 或修改现有 App。

使用当前平台的官方空白脚手架。先阅读当前 agentic-app 的构建/发布契约和
LLM capability 说明，保留 package.json 和最小前端壳，构建层由平台注入。
把本仓库 examples/anygen/app/ 整棵源码作为参考；如果你无法直接访问这些附件，
请先索取内容，不要凭空猜 SDK。

最终契约：

1. manifest 保留 `manifestVersion: "0.1"`、`kitVersion: 1`，
   capability 仅 `["llm"]`。
2. 精确暴露两个动作：
   - `POST /api/v1/chat/completions`
   - `GET /api/v1/models`
3. Chat 使用 `useAnygen().llmEndpoint()` 返回的 `{baseUrl, apiKey}`，
   按固定模板通过原生 fetch 请求内部 `/chat/completions`，返回完整
   `chat.completion` 对象。不要替换成 `ag.complete()` 简化结果。
4. `model`、非空 `messages` 必填；保留 OpenAI 字段和消息扩展字段，包括
   tool_calls、tool_call_id、tools、tool_choice、reasoning_effort、
   response_format、max_tokens/max_completion_tokens、结构化 content。
   不加 messages 条数上限，不截断、不摘要、不重排消息。
5. 只接受 `stream` 省略或 false，true 在调用模型前明确拒绝。
   不声称内部流式能穿透外层 buffered publication，不在 App 内模拟 SSE。
6. 不额外写死 30 秒超时，不做模型 POST 自动重试；平台外层预算仍然有效。
   内部 apiKey/baseUrl 不得返回、记录或落盘，也不要嵌入外部 sk-ag Key/Cookie。
7. Models 使用 `ag.listModels()`，返回 `{"object":"list","data":[...]}`。
   不缓存固定的模型 ID/数量；积分不做第三个 action，走平台 key/verify。
8. 动作用命名导出的 `Api(Post('/v1/chat/completions'), ...)` /
   `Api(Get('/v1/models'), ...)`；方法和字面路径必须是 Api 首参数。
   handler 不写内联参数类型，模块顶层不执行副作用。
9. 用当前平台支持的发布流程（原流程为 `app_run mode=publish`）完成构建。
   发布不是只写源码；报告构建 diagnostics、版本和真实动作表。
   免费检查模型列表；不要执行 chat/501 条消息的付费自检。
10. 如 schema/framework/runtime 契约已变化，报告差异，保留上述行为契约，
    不通过增加权限、更换目标 Agent 或调用管理员接口解决。

最终报告：claw token、App 槽、版本/构建状态、两条动作、capabilities、
模型列表结果、是否删除了消息条数限制、凭证未泄露。
只有拿到 publication 真实返回才提供外部 ref；不要把内部页面 ref 当成它。
将所有未执行的付费测试标为“未验证”，不要报告虚构的闭环成功。
