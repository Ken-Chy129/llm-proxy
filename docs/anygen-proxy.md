# 复刻一个新的 AnyGen LLM 代理

## 结论与验证范围

采用 **两动作 Agentic App + 平台 `sk-ag` Key 精确授权**，终点是
**可直连的 Base URL + Key + 一次 Chat 验收**。仓库只是存放创建工具，
不要求配置、启动或重启 llm-proxy。

`provision` 已将创建 Agent、上传固定 App 包、建立 publication、临时 hook 清理、
创建/复用 Key、精确授权和直连验收串起来。正常流程不需要人手动创建 App、
让大模型重写代码或复制 publication ref；人负责认证及明确创建/测试授权。

本手册于 **2026-09-14** 整理，并通过私人默认槽实例进行现网验证。
首次 provision 现网试跑已完成
新 Agent 创建、固定源码部署（diagnostics 为空）、两动作核验、publication 创建和
临时 hook 吊销。首次创建专用 Key 的 POST 返回 HTTP 403；只读确认未创建后，
经用户重新授权，补充同源 Origin/Referer 再创建成功，明文仅存入本地私有凭证文件。
两动作 grant、Key 验证及新 App 的模型列表均已通过（该次返回 36 个模型）。
随后一次非流式 Chat 冒烟返回预期正文，状态已为 verified；输入 13、输出 8 token，
耗时约 2.6 秒。测试实例标识、凭证和完整运行记录不随仓库发布。
当前账户权限、UI、模型集合和线上部署版本必须在复刻时重新核实。

本仓库提供：

| 材料 | 用途 |
|---|---|
| [anygen.py](../scripts/anygen/anygen.py) / [provision.py](../scripts/anygen/provision.py) | Python 3.9+；provision 自动创建直连 API，其余命令可单独检查/排障 |
| [完整作者源码包](../examples/anygen/app) | manifest/package/React 入口/两动作；平台注入构建工具和 runtime |
| [创建 App 的任务文本](../examples/anygen/create-app-prompt.md) | 平台契约变化时的人工/Agent 辅助构建方案，不是正常流程必经步骤 |
| [anygen-proxy Skill](../.agents/skills/anygen-proxy/SKILL.md) | 运行 provision、解释权限边界及处理异常，不修改本地代理配置 |

## 0. 推荐：直接运行 provision

运行环境：macOS/Linux、Python 3.9+，不需要本机安装 Node、OpenAI SDK 或启动代理服务。
创建命令目前只支持新的私人默认槽，不接受已有 claw 或命名槽覆盖。
底层真实端点、已有实例的排查方法见后文。

### 只看计划

从仓库根运行；不访问网络、不读取凭证、不创建状态目录：

```bash
python3 scripts/anygen/anygen.py provision \
  --name my-anygen-proxy --state-dir /private/path/my-anygen-proxy
```

### 方式 A：owner 登录态，自动创建专用 Key

在本地安全设置 `ANYGEN_SESSION` 和 `ANYGEN_CSRF_TOKEN`（不在聊天/日志中粘贴）。
以下命令会创建资源，但不调用模型：

```bash
python3 scripts/anygen/anygen.py --gateway-auth cookie provision \
  --name my-anygen-proxy --state-dir /private/path/my-anygen-proxy \
  --create-key --apply
```

### 方式 B：复用已有平台 Key

本地设置 `ANYGEN_LLM_KEY`，并确认它的 Key ID。Bearer 管理入口需当前线上支持；
也可以显式选 Cookie 模式。脚本检查 owner/Key 掩码及精确动作权限，不从 Key 字符串猜 ID：

```bash
python3 scripts/anygen/anygen.py provision \
  --name my-anygen-proxy --state-dir /private/path/my-anygen-proxy \
  --key-id YOUR_KEY_ID --apply
```

不复用已有 App，不替换其他 App 的权限。专用 Key 在创建时只添加新 App 的两条
精确 feature；这仍不能消除平台 Key 的其他入口能力，见下方安全边界。

### 验收与交付

免费检查返回 `ready_unverified`、实际模型列表及 credentials 文件位置。
选择列表里的模型并确认一次消费后，用**相同 state-dir、name、Key 模式和认证**
再次运行，加上：

```bash
--allow-charge --model MODEL_FROM_MODELS --max-tokens 1024
```

也可以在首次创建时一并传入，脚本会先检查该模型在真实列表中，再调用一次。
默认短提示词为 `Reply only: ANYGEN_PROXY_OK`。只有标准 `chat.completion` 且有非空
正文才标为 `verified`；空结果、仅 tool call 不满足这次文本冒烟目标。
每个状态目录最多执行一次自动模型调用；已验证的运行不会因重复执行再扣积分。
模型或预算不同也不会复用旧测试结果冒充新测试。

状态目录父目录须已存在；脚本创建叶目录为 0700，里面文件为 0600：

| 文件 | 内容 |
|---|---|
| `credentials.json` | `base_url`、`api_key`、`api_key_id`；是明文敏感文件，只有 owner 可读 |
| `result.json` | 不含密钥的 URL、claw token、验收状态和凭证文件位置 |
| `state.json` | 资源 ID、包指纹、认证指纹、已完成步骤、pending 标记；不含明文认证 |
| `run.lock` | 防止同一状态目录被并发执行 |

只有 credentials.json 含 Key；终端、状态日志和源码包都不含明文 Key/Cookie。
文件权限不等于加密：该目录仍应放在本机受保护的位置，不放进共享盘或自动上传的目录。
不要删除/分享状态目录；丢失 journal 而留下凭证时脚本拒绝重新创建。
相同状态目录可在免费检查后补验收、或在只读请求失败后继续已完成步骤。
**不是无限自动恢复**：写入返回不明、构建诊断、smoke 失败后保留 pending 并停止；
不能删 pending 或换状态目录盲重试。检查 state.json 中资源 ID，再核实远端结果。
更换 owner 登录凭证/Key、名称或模板后也拒绝自动接管，需先审查已有资源，
不要假装新认证和旧认证必属于同一账户。

获取输出后，直接将 credentials.json 中的 `base_url` 和 `api_key` 交给你自己的
OpenAI-compatible 客户端即可；不依赖 llm-proxy，也不需要运行本地常驻服务。
`config` 仅为可选附加工具，不属于 provision 流程。

当前验证：固定模板通过本地 builder 组装及首次平台部署，部署 diagnostics 为空，
真实动作表包含预期两动作。本地完整依赖安装曾因网络中断未完成，但平台部署已通过。
新实例已完成 Key 创建、授权、免费检查和一次真实生成验收。
首次 Key 创建缺少 Origin/Referer 返回 403，补齐后成功，提示请求来源校验差异；
未做单头对照或网关日志确认，不将其定性为具体某项 CSRF 校验。Cookie 请求现已
携带固定同源头，Bearer 请求不携带；仍不自动重试写入或放宽权限。

## 1. 运行时链路与凭证分层

```text
应用 / OpenAI-compatible 客户端
  │ Base URL + owner 的平台 sk-ag Key
  ▼
AnyGen publication 网关：ref → App；验证 Key、owner、action grant、限流
  ▼
App 的 POST /api/v1/chat/completions
  │ useAnygen().llmEndpoint() 取得内部 baseUrl / apiKey
  ▼
AnyGen LLM bridge → 模型供应商 → 扣 App owner 积分
```

使用该 App 的完整 base URL 即可直连；llm-proxy 是可选的另一层代理。
`/api/v1/chat/completions` 是 App 自定义 action，不是 AnyGen 域名根上的固定公共接口。
底层 `/api/page/llm_proxy/v1/*` 使用内部 LLM credential，不能拿外部 `sk-ag` 直接替换。

| 标识 | 从哪里获取 | 用途 |
|---|---|---|
| claw token | 新 Agent 的真实元数据 | 管理动作的 `claw_token`；不是 URL 中的 publication ref |
| App 槽 | 默认省略；命名槽须平台支持 | 管理参数 `app`；命名槽路径和授权坐标也带槽名 |
| publication ref | 发布状态 URL 或创建 App hook 的真实响应 | 外部 URL 的 `/app/<ref>` |
| Key ID | owner 平台 Key 列表/`listClawAppKeyGrants` | `setClawAppKeyGrant.api_key_id`，不是密钥本身 |
| `sk-ag…` | owner 的平台 API Key 创建结果/已有安全凭证库 | 外部 Bearer 认证；只注入环境变量 |
| `mak_…` | `createClawAppHook`，完整 URL 只返回一次 | 固定动作 hook；不是 OpenAI SDK 的 base URL |
| 内部 LLM credential | App 请求内 `ag.llmEndpoint()` | 不可返回、持久化或提供给外部调用方 |

**安全边界：** `whole_app=false` + 两动作 grant 只约束 App machine 入口。
它不会清除该平台 Key 的其他功能权限。本地服务端 `OpenAPIAnyClawGateway`
还存在“验证平台 Key 后以 owner 身份分发 gateway”的入口，不能将 action grant
解释为整把 Key 的全平台沙箱。因此只给自己/可信服务保存平台 Key；
面向第三方时分发 llm-proxy 的 Key，设置适当额度，不泄露上游凭证。
脚本支持独立管理 Key 的环境变量，但也不意味着运行 Key 在平台上自动失去管理能力。

## 2. App 契约与辅助构建方式

正常路径由 provision 上传 [app/](../examples/anygen/app) 中固定作者源码，
使用 `ag.llmEndpoint()` + 原生 fetch，保留完整协议且不自动重试。
平台负责注入 Modern.js、TypeScript 和 `@anygen/*`；不把平台 harness 固化到 bundle。
该模板不依赖 OpenAI SDK。原 [SDK 示例](../examples/anygen/llm.ts) 仅供参考，不会被 provision 打包。

平台变化需要人工调整，或用户明确要求用 AnyGen Agent 构建时，才使用以下辅助路径：

1. 登录目标 owner 的 AnyGen，确认 Agentic App 能力可用。
2. 创建独立私人 Agent，并记录其 claw token。不要复用已经装了业务 App 的默认槽。
3. 将[构建任务文本](../examples/anygen/create-app-prompt.md)里的目标替换，
   连同参考动作/manifest 提供给它，使用当前平台空白脚手架构建。
4. 原流程使用 `app_run mode=publish`；以当前平台工具契约为准。
   等构建完成，保存版本、diagnostics、真实动作列表，不只看 Agent 说“已发布”。

最终必须有：

```text
POST /api/v1/chat/completions
GET  /api/v1/models
manifestVersion = "0.1"
kitVersion = 1
capabilities = ["llm"]
```

关键约束：

- 使用命名导出的 `Api(Post('/v1/chat/completions'), ...)` 和
  `Api(Get('/v1/models'), ...)`；平台加 `/api` 前缀，文件名不参与路径。
- 方法及字面路径放在 `Api` 首参数；不要包 helper。handler 不写内联参数类型，
  模块顶层不执行副作用。这些是原静态动作提取器的兼容约束，应核对当前版本。
- Chat 返回标准 `chat.completion`，不使用只返回 `{text, usage, ...}` 的
  `ag.complete()` 简化封装；tools 和合法扩展字段必须保留。
- 不添加 `messages.max(500)` 或其他任意条数阈值，不静默截断/重排消息。
  上下文 token 窗口、请求体大小、平台预算仍然存在。
- Models 用 `ag.listModels()` 动态返回，不硬编码“17/31 个模型”或某个 ID。
- 此参考 App 只接受 `stream:false` / 省略；true 在入参校验时拒绝。
  内部 bridge 流式不代表外部 publication 可增量返回。
- 不增加积分 action；余额使用平台接口。不要将 owner Key/Cookie 嵌入 App。

脚本实际使用管理动作
`deployClawApp {claw_token, app?, bundle, confirm?}` 接收 base64 tar.gz 源码包，
但需要完整、与当前平台一致的脚手架。`ok` 不等于运行时已构建可访问，
diagnostics 也可能非空；模板 fork 还需要独立确认。
provision 打包的 tar 根直接是 manifest.json/package.json/src/api，而不是再套一层 app/。
它先检查新槽为空，部署后要求 diagnostics 为空、两动作表精确匹配；不覆盖或 fork 既有 App。

## 3. 准备平台 Key 和管理认证

使用 owner 的专用、user-created 平台 API Key。旧 UI 路径是
“设置 → 集成 → API 密钥”，当前路径可能变化；不要使用沙箱 system Key。
新建时把明文直接保存到本地安全凭证库，记录 Key ID，不贴到聊天或终端历史。
调用使用的 Key 和被授权的 Key ID 必须是同一条。

以下命令均从仓库根运行。先安全地注入 `ANYGEN_LLM_KEY`；也可用
`--key-env OTHER_ENV_NAME`。脚本不会读取 `.env`、axon-cli 凭证库或浏览器存储，
也不会接收明文 Key 参数。

例如在交互式 Bash 中可隐藏输入（不要启用 `set -x`）：

```bash
read -r -s -p "AnyGen platform key: " ANYGEN_LLM_KEY
export ANYGEN_LLM_KEY
```

管理 gateway 有两种认证：

| 模式 | 入口 | 注意 |
|---|---|---|
| 默认 Bearer | `POST /v1/openapi/gateway` | 当前本地 handler 有实现；旧任务的 Bearer 尝试不足以证明线上成功，先只读检查 |
| 显式 Cookie | `POST /api/v1/gateway_anygen` | 旧任务实际成功写入的路径；需有效 owner session 和 CSRF |

通用 envelope 是 `{"method":"…","body":{…}}`。
脚本同时检查 HTTP 和业务 `code`；HTTP 200 + `code:4101` 不是成功。

```bash
# 只读：发布状态、动作表、owner Key 的 App grant
python3 scripts/anygen/anygen.py inspect --claw NEW_CLAW_TOKEN

# 可选择另一条 owner 管理 Key；运行 check/smoke 仍使用 ANYGEN_LLM_KEY
python3 scripts/anygen/anygen.py --gateway-key-env ANYGEN_OWNER_KEY \
  inspect --claw NEW_CLAW_TOKEN
```

若 Bearer gateway 不支持或拒绝，先停止。经用户选择，用户自行在本地设置
`ANYGEN_SESSION`、`ANYGEN_CSRF_TOKEN`，再运行：

```bash
python3 scripts/anygen/anygen.py --gateway-auth cookie inspect --claw NEW_CLAW_TOKEN
```

Cookie 只用于管理 gateway，不发送给模型/积分端点。不要自动收集浏览器 Cookie、
要求用户贴完整 curl 登录头、使用管理员替代 owner 或因为失败扩权。

## 4. 创建 publication：容易漏掉的一步

**App 内容构建发布 ≠ publication 数据行 ≠ Key grant。三者都需要。**
内部构建页面 ref 和外部 publication ref 在旧任务中就不一样，
不能把 `app_run` 随手返回的任意 ref 拼入 machine URL。

先检查 `getClawAppPublishStatus` 与真实返回 URL。已有可用 publication 就复用。
对于新的私人默认槽，没有绑定飞书 bot 也可使用机器面。
不要为了机器调用去打开人访问的发布开关：`setClawAppPublish` 是飞书网页应用
发布面，会检查 bot，甚至涉及飞书应用配置。

缺失 publication 时，旧流程利用“首次创建 App hook 自动创建 publication”。
这是凭证创建操作，必须先明确确认目标；不是免费查询。使用 owner 管理 gateway：

```json
{
  "method": "createClawAppHook",
  "body": {
    "claw_token": "NEW_CLAW_TOKEN",
    "name": "llm-proxy-bootstrap",
    "action": "POST /api/v1/chat/completions"
  }
}
```

这是 **App action hook**，不是 Agent “集成”里触发聊天的普通 Webhook。
成功后：

1. 不展示/记录完整含 `mak_…` 的 URL；只提取 `hook.id` 和 URL 的 publication ref。
2. 核对发布状态及目标，确认 URL 属于本次 App。
3. 若只用 `sk-ag`，通过以下动作吊销**本次新建的临时 hook**；publication 数据行保留。
   如果用户确实要保留 hook，则单独安全保存完整 URL。

```json
{
  "method": "deleteClawAppHook",
  "body": {"claw_token": "NEW_CLAW_TOKEN", "id": "ID_FROM_THIS_CREATE"}
}
```

不需要调用 hook 做检查——它绑定模型 action，会扣积分。
创建失败或响应丢失时不要盲重试，先用 `listClawAppHooks` 检查，
避免重复占用配额；旧流程的首次 `publication not found` 曾在重试后恢复，
但“读副本延迟”只是当时推断，不是所有 404 的诊断结论。
已有 publication 停用时，创建 hook 不保证重新启用；停止并确认生命周期操作。

当前本地代码已有命名槽：URL 为 `/app/<ref>/<slot>/api/v1`，
管理请求带 `app`，grant 坐标为 `claw/slot`。但槽位公开启用、机器路由以及
hook 配额有额外约束，不能把默认槽的 bootstrap 无条件套过去。
本次不声称完成命名槽线上验证；首次复刻推荐新的私人默认槽。

## 5. 精确授予两个动作

动作名必须以 `listClawAppActions` 结果为准。计划格式：

```json
{
  "method": "setClawAppKeyGrant",
  "body": {
    "claw_token": "NEW_CLAW_TOKEN",
    "api_key_id": "KEY_ID",
    "whole_app": false,
    "actions": [
      "POST /api/v1/chat/completions",
      "GET /api/v1/models"
    ]
  }
}
```

默认槽会生成 `app:<claw>:<METHOD /api/path>` feature。
不是 `*`，也不是仅给 `POST /complete`。`setClawAppKeyGrant` 替换**本 App**
的 grant，保留其他 App/平台 features；不能手动用 Key REST 全量 features 覆盖来代替。

```bash
# 完全离线预览，不读取凭证、不发 HTTP
python3 scripts/anygen/anygen.py grant --claw NEW_CLAW_TOKEN --key-id KEY_ID

# 确认目标后执行：动作预检 → owner Key 查找 → 写入 → 读回
python3 scripts/anygen/anygen.py grant --claw NEW_CLAW_TOKEN --key-id KEY_ID --apply
```

相同授权为 no-op；存在不同 grant 会停止。
确认替换旧权限后才额外加 `--replace-existing`。不要并发修改同一 Key；
这段读取/写入没有服务端 CAS 保证。写入响应丢失或读回不同，
先 inspect，不自动重试/回滚。
Cookie 模式把 `--gateway-auth cookie` 放在 `grant` 前；
命名槽需为所有相关命令一致添加 `--slot SLOT`。

## 6. 分层验收：不要把“200”当闭环

### 免费模型和积分检查

```bash
python3 scripts/anygen/anygen.py check --ref NEW_PUBLICATION_REF
```

脚本分别请求：

```text
GET https://www.anygen.io/v1/openapi/key/verify
GET https://www.anygen.io/v1/openapi/anyclaw/app/<ref>/api/v1/models
Authorization: Bearer <runtime platform key>
```

前者验证 Key，并返回 `verified / user_id / credits`；余额在 wire 上是字符串。
后者返回 `{"object":"list","data":[{"id":"…",…}]}`，不调用模型。
检查结果中 `chat_tested:false` 是刻意保留的：能查模型不等于能完成生成。

### 一次付费 Chat 冒烟

从刚返回的列表选一个适合低成本验证的模型；不要照抄旧任务的模型 ID。
用户确认后执行：

```bash
python3 scripts/anygen/anygen.py --timeout 180 smoke \
  --ref NEW_PUBLICATION_REF --model MODEL_FROM_MODELS --allow-charge
```

固定一条短消息、`stream:false`、默认 `max_tokens:1024`。
可在确认费用预算后用 `--max-tokens` 调整；不自动遍历模型或重试。
通过条件是标准 `chat.completion` 和非空正文/有效 tool call，
不是 HTTP 200。空 choices、只有 reasoning、`finish_reason:length` 且正文空
不能证明代理可供实际使用。16/64 输出 token 对推理模型可能不足，
这与“501 条 messages 能通过 schema”是两个不同测试目标。

`--timeout` 是诊断客户端 socket 超时，不会修改平台 deadline，也不是精确的总耗时上限。
脚本不输出完整 prompt/响应正文，不保存凭证，拒绝 HTTP 重定向，
且响应体诊断预算为 4 MiB；这些不是上游平台限制。

## 7. 可选附录：接入 llm-proxy（不属于创建/验收流程）

```bash
python3 scripts/anygen/anygen.py config \
  --ref NEW_PUBLICATION_REF --model MODEL_FROM_MODELS
```

输出可以人工合并的片段：

```yaml
anygen:
  enabled: true
  base_url: "https://www.anygen.io/v1/openapi/anyclaw/app/NEW_PUBLICATION_REF/api/v1"
  api_key_env: "ANYGEN_LLM_KEY"
models:
  - name: "MODEL_FROM_MODELS"
    providers: [anygen]
```

不要用输出覆盖现有 `config.yaml`，保留其他 providers、models、series 和服务配置。
显式填写新 ref；当前 executor 的空 base URL 兜底仍保留原实例地址以兼容旧配置，
它不应作为新账号的默认接入点。

启动进程/容器需能读取 `ANYGEN_LLM_KEY`。Compose 已声明这个变量；
原生 Go 进程不会因为目录里有 `.env` 就自动加载它。
使用自定义 `api_key_env` 时，也要自行将同名环境变量传给容器。
当前配置只有一个 `anygen` provider：更换 base URL 会切换它的上游，
不等于同时新增第二个 AnyGen 账号池。

启动后核对：

- Dashboard 的 AnyGen catalog / Quota；余额来自平台 key/verify，不属于 OpenAI 计费 API。
- 上游同步只更新 **catalog**，不会自动发布所有模型；`models` 路由表决定对外模型。
  在配置中添加，或在 Dashboard 明确发布需要的模型。
- 客户端访问本代理 `/v1/models` 确认模型已发布。若想排除 fallback 干扰，
  可在受控测试中用现有 provider override，例如 `MODEL_FROM_MODELS@anygen`。
- 经用户确认再做代理侧 Chat/Responses 冒烟。上游直连成功不代表部署进程已收到新环境变量。

AnyGen 上游仍非流式。本仓库 `Chain` 会把完整 Chat 结果转换成 SSE，
Responses 层也可转换为 typed Responses SSE（包含 function calls）。
这只兼容客户端协议，首包仍须等待模型完整生成，不是 token 级实时流式。

## 8. 限制、排障和生命周期

| 现象 | 优先检查 |
|---|---|
| gateway HTTP 200、`code:4101` | 登录/Key 是否有效、管理入口是否上线；不是授权成功 |
| 401/403 | 外部 Key 与 Key ID 是否同一条；owner、功能开关、精确 action；不要改成 whole-app 碰运气 |
| 404 / `publication not found` | publication 行/启用态、实际 ref、槽位、动作表；不要直接认定是模型缺失或复制延迟 |
| `/models` 成功、chat 被拒 | 是否只给了 GET grant；POST schema、模型能力/余额 |
| `messages maximum:500` | App 的人为 Zod 上限；去掉条数阈值，不截断 tool-call 历史 |
| HTTP 200 但没可用输出 | choices、正文/tool calls、输出预算、reasoning、filter；不能报告通过 |
| `code:1,msg:Failed` | 外层业务信封，保存安全的 HTTP 状态/耗时/logid；并非标准 OpenAI 结果 |
| 429 | App Key 分钟/日限制或模型上游限制；有界退避，不多建 Key 绕限 |
| 接近 30/50/60/300 秒失败 | 记录客户端总耗时/TTFB、HTTP/logid，逐层区分 deadline |

本地 mino_server 可确认的预算：普通 portal 30 秒，能力桥 300 秒，
前端来源的幂等读 50 秒；带 `llm` 能力的 POST 通常选能力桥预算。
这不证明线上请求一定能跑满 300 秒：发布路由、FaaS、AGW、SDK、调用方
可能更早结束；旧任务关于“没有 30 秒”的表述不能当作 SLA。

本地机器入口缺省限流 60/min，平台 Key 可有自己的 RateLimit/DailyLimit；
不要把旧 Key 的“每日不限”推广给新 Key。hook 上限 5，当前多槽版本按整个
publication/Agent 共享，不是每槽各 5 条。查询 models 不做模型推理，
但仍可能占用机器 API 速率窗口。

轮换 Key：新 Key 创建/授权 → 免费检查 → 受控冒烟 → 更新代理环境并按部署流程重启
→ 验证 → 再吊销旧 Key。不要先撤销唯一工作凭证。
要撤销某 App 的 grant，可明确调用 `whole_app:false, actions:[]`；
它不会删掉其他 App 的权限。删除 App、停用 publication、删除 Key/hook、
移除路由或关闭功能开关都可能让接口失效；不要承诺永久可用。

当前只有 Chat 和 Models 两动作，不能因此声称提供 Images API。

## 9. Skill 和测试

在本仓库任务中调用 `$anygen-proxy`；如果当前客户端尚未发现新增 Skill，
直接让它读取 `.agents/skills/anygen-proxy/SKILL.md`，或在仓库中新开任务。
Skill 引用仓库内脚本/手册/完整模板，拷贝 Skill 单一目录到别处不会自包含。
它不会自动授权线上变更或费用，也不依赖某台机器的 axon-cli 凭证路径。

```bash
python3 -B -m unittest discover -s scripts/anygen -v
```

这些测试使用模拟服务响应/传输，不访问 AnyGen、不消耗积分。
模板本地组装检查和 Python 流程测试都不能替代目标平台部署及一次真实生成验收。

## 10. 本仓库实现导航

- Key、Models 与余额：[internal/executor/anygen.go](../internal/executor/anygen.go)。
- catalog 与服务模型分开：[internal/router/apply.go](../internal/router/apply.go)。
- 缓冲结果转流式：[internal/executor/stream_adapter.go](../internal/executor/stream_adapter.go)。

平台实现会随版本变化，以目标账户实际返回和测试结果为准。
