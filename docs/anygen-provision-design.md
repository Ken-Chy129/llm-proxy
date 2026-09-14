# AnyGen provision：实现与验收约定

目标：仅创建新的私人默认槽 App，交付直连 AnyGen 的 Base URL + 平台 Key，
以一次标准 Chat Completions 返回非空正文作为端到端验收。不配置、不启动 llm-proxy。

命令：

```bash
python3 scripts/anygen/anygen.py provision --name my-proxy --state-dir /private/path/my-proxy
python3 scripts/anygen/anygen.py --gateway-auth cookie provision \
  --name my-proxy --state-dir /private/path/my-proxy --create-key --apply
python3 -B -m unittest discover -s scripts/anygen -v
```

默认离线计划；`--apply` 才操作线上。复用 Key 需提供 `--key-id`，
新建 Key 用 `--create-key` 且需 Cookie 管理认证。一次 smoke 另需
`--allow-charge --model <已确认模型>`；不自动选择昂贵模型、不自动重试 POST。
未指定付费选项时只完成免费检查，状态为 ready_unverified，不是验收通过。

实现顺序与验收：

1. 模板与打包：保存完整作者源码（manifest/package/React 入口/两动作），不携带
   凭证、依赖目录或平台 harness；tar 根直接是 manifest.json，不带 app/ 前缀。
   用平台 builder 的组装/类型检查验证。包指纹固定并记录进状态。
2. 状态与 transport：独占运行锁、0700 状态目录、0600 原子写文件；状态日志不含
   Key/Cookie/hook secret。Key 单独保存。每个非幂等写入之前持久化 pending，
   响应不明停止，重跑不得自动重复写入。不自动删除远端 Agent 或轮换凭证。
3. 创建流程：认证预检 → 创建 Agent → 检查空槽 → 部署包 → 校验 diagnostics/
   动作表 → 创建临时 App hook 获得 publication → 撤销本次 hook →
   创建/复用 Key → 精确 grant 并读回 → 免费模型/Key 校验 → 可选一次 smoke。
   只操作本轮返回并记入状态的 claw；不接受覆盖现有 App。
4. 测试与文档：用有状态 Fake 完整演练正常路径、断点继续、未知写结果、
   空模型输出、认证/Key 不匹配、配置不变、权限/脱敏与文件安全。
   离线通过不冒充线上模板部署或真实模型调用通过。

恢复边界：成功完成的步骤可跳过；上一次进程留下 pending 时先只读核实。
v1 不自动接管“结果不明的 createClaw/createKey/createHook/deploy/smoke”。
保留资源 ID 与安全诊断供人工定位；修复后继续需明确确认，不用盲重试获得假幂等。

Python 使用标准库及现有 Failure/Client 约定；新增 provision 与本地状态模块
和旧 inspect/grant/check/smoke/config 分离。既有命令行为保持兼容。
