---
name: anygen-proxy
description: 创建可直连的 AnyGen LLM API，自动创建 Agent、部署固定 App、建立 publication、授权平台 Key，交付 Base URL 和安全保存的 Key 并验证链路。用于复刻或排查 AnyGen 代理，不要求启动或配置 llm-proxy。
---

# AnyGen 代理复刻

本 Skill 依赖整个 llm-proxy 仓库，不是可单独拷走的独立安装包。
从本目录向上三级是仓库根目录。先阅读
[接入手册](../../../docs/anygen-proxy.md)，按用户要求选择“整理/检查”
或“实际创建”，历史成功不代表本次已授权生产写入。

## 执行

### 新建：优先使用 provision

目标只到 **URL + Key + 一次直连 Chat 验收**。仓库是工具载体，不修改 config.yaml、
不启动/重启 llm-proxy，也不把配置片段作为完成条件。

1. 收集实例名称、专用状态目录、认证方式；复用 Key 提供 Key ID/环境变量名，
   新建 Key 选择 Cookie 模式。不需要人手工创建 Agent、写 App、提取 ref 或建 hook。
2. 执行离线计划：`python3 scripts/anygen/anygen.py provision --name NAME --state-dir PATH`。
   脚本使用固定生产域名、新的私人默认槽和仓库固定模板；不接受已有 claw 覆盖。
3. 说明计划中的创建、部署、临时 hook 创建/吊销、Key 权限和凭证文件位置。
   用户可以一次授权整个明确范围，不需要每一步重复询问。实际执行添加 `--apply`，
   复用 Key 加 `--key-id ID`；新建 Key 用 `--gateway-auth cookie`（子命令前）和 `--create-key`。
   本地设置认证，不把 session/Key 粘进聊天，不自动抓取浏览器凭证。
4. 免费检查返回 `ready_unverified` 和模型列表。用户已确认测试模型及一次测试预算时，
   同一命令加 `--allow-charge --model MODEL`，或首次执行就带上这两个选项。
   `verified` 只在直连返回非空正文时成立；不自动选昂贵模型、不重试模型调用。
5. 同一 state-dir 继续已完成步骤。出现 pending 表示前次写入结果需要人工核实，
   不删状态、不换目录重试、不清 pending 强行推进。按手册恢复边界处理。
6. 交付 base_url、credentials.json 的安全本地路径、模型和验收结果。
   不回显密钥。当前只做过离线验收时必须明确说明，不冒充现网成功。

完整作者模板在 [examples/anygen/app](../../../examples/anygen/app)，不需要 LLM 重新生成。
模板使用 ag.llmEndpoint + 原生 fetch，保留标准协议，不依赖 OpenAI SDK。

### 已有实例：检查或修复

1. 确认 owner、目标 claw、App 槽（省略=默认）、publication ref、Key ID
   与凭证环境变量名。claw token、内部页面 ref、外部 publication ref
   不可混用；新建默认用新的私人 Agent，发现已有 App 不覆盖。
2. 如需创建 App，使用
   [构建任务](../../../examples/anygen/create-app-prompt.md)和同目录示例。
   走可用的产品工具/授权浏览器或已核实的部署 API。平台脚手架和登录步骤
   留在平台执行，不猜 UI selector、API、bundle 依赖或当前模型 ID。
   不把 `deployClawApp.ok` 或 Agent 自述当作对外发布成功。
3. 用 `python3 scripts/anygen/anygen.py inspect --claw CLAW` 检查两动作及发布态。
   网络请求前说明环境、目标和用途。脚本使用生产域名；不拿它测试 PPE。
   Bearer gateway 在旧线程里未验证成功；先只读验证，不自动切 Cookie。
   如需 Cookie，用户自行在本地设置 `ANYGEN_SESSION` / `ANYGEN_CSRF_TOKEN`，
   再显式 `--gateway-auth cookie`；不索取聊天里的 Cookie，不读取浏览器密钥库。
4. publication 缺失/停用按手册处理。只有用户明确授权才创建临时 hook；
   记录真实 ref 与 hook ID，核对结果，再删除本次临时 hook。
   不将此 hook 调用一次作为检查（它会触发模型）；不盲重试创建。
   命名槽的开启与机器路规则有版本差异，未验证前不能自动扩展。
5. `grant --claw CLAW --key-id KEY_ID` 默认离线预览。
   用户确认目标后才能添加 `--apply`。有旧权限冲突先报告差异，
   不擅自加 `--replace-existing`。精确授权只保护 App machine 入口；
   平台 Key 可能有其他 owner 权限，不能当作安全的第三方分发 Key。
6. 用 `check --ref REF` 读取真实模型/积分；选择返回的 ID。
   `smoke --ref REF --model MODEL --allow-charge` 需要明确付费测试授权，
   一次请求后停止，不重试/遍历模型。非空正文或有效 tool call 才算可用输出。
7. 到直连验收完成即停止。`config` 仅为独立可选工具，不属于本 Skill 创建流程。

所有命令从仓库根运行；全局参数在子命令前，命名槽用 `--slot SLOT`。

## 交付

报告已完成/未验证的步骤、Base URL、两动作、Key ID/环境变量名、模型 ID、
凭证文件位置和失败证据；不报告明文凭证、完整 hook URL 或内部 LLM credential。
这套参考 App 的上游是非流式，llm-proxy 的 SSE 是完整结果后的适配，
不是低延迟 token streaming。不得声称“无限等待”“全模型可用”或“只两文件一键部署”。
