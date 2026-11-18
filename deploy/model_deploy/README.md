# 模型部署管理与 GPU 执行器

modelGateway 管理版本定义、部署任务、操作互斥和回执；GPU 主机上的独立执行器执行启停、预热、验收、切换和回退。模型加载、LoRA 兼容性、Compose 模板及输出验收插件留在模型仓库。生成请求进程不执行 Docker，也不等待模型加载。部署记录保存在云端 PostgreSQL，GPU 机器只持有制品缓存、运行状态和 Gateway 凭据，不需要数据库密码。

本版先接入 VMind-Image，覆盖独立单 Worker、单 model_id、单 GPU 的新部署及同主机 LoRA 切换。新机器可以运行同一执行器；跨主机切换、多副本滚动、共享容器/共享卡联动及 VMind-Video 的专用适配不在本版。复用原卡会有停机预热时间；有空闲卡时先验候选再排空旧实例。不会自动构建镜像或因代码合并自动部署模型。

## 组件和事实来源

| 内容 | 所属位置 |
| --- | --- |
| 完整版本定义、目标和当前部署状态 | `model_gateway.model_deployments` |
| 操作状态、领取记录、结果和执行日志 | `model_gateway.model_deployment_tasks`；日志存 `events` JSON 数组 |
| 任务提交、领取、回执、恢复与鉴权 | Gateway 管理 HTTP API |
| Docker/GPU 检查、候选验收、排空和切换 | 本目录 `manage.py`，在目标 GPU 主机执行 |
| 模型加载、LoRA 描述、输出校验和部署模板 | [VMind-Image 部署配置](https://github.com/wgdl666/VMind-Image/tree/main/deploy/model_deploy)；合并前使用对应 PR 分支 |
| 模型请求、实际执行归属和容量 | 原 `model_gateway` schema，沿用原登记/drain/生成接口 |
| 镜像与权重 | 固定 digest 的 ECR 镜像及固定 revision/SHA256 的 HF/S3 制品 |

`model_id` 是调用方入口，等于 model_key；`version_id` 是版本 canonical JSON 的 SHA256；`deployment_id` 是实例身份；`task_id` 是一次操作的幂等键。切换 LoRA 保留 model_id，换 deployment_id 和 version_id。无 release/eval 类型：需要固定评测版本时使用独立 model_id 并保持不更新。model_id 不构成权限隔离。

只保留上述两张表：版本定义放在部署记录的 `manifest.version` 中，不另建版本表；任务的 `events` 保存按顺序追加的回执，不另建事件表。未完成任务就是模型和 GPU 的操作占用，不另建锁表。提交和结束任务使用短数据库事务串行检查占用，不在模型加载期间持有数据库事务。

版本不包含目标机器、端口和验收用例。API 接收 `manifest_json` / `version_json` 的 canonical JSON 字符串，服务端校验内容对应关系并按原字节计算 SHA256，避免不同语言 Unicode/数值序列化造成摘要变化。客户端使用 `json.dumps(..., sort_keys=True, separators=(',', ':'))`。实际 Compose 原文及 manifest 存 DB，解析后的凭据不入库。

## Gateway 初始化（数据库操作只在服务端）

1. 保持 `model_deployments_enabled: false`。如已有执行中的部署任务，先完成或恢复到安全终态，再禁用；不要靠关接口来中断执行。
2. 通过已有数据库迁移入口，以迁移账号执行 [model-deployments.sql](../migrations/model-deployments.sql)，复用已有 `model_gateway` schema。使用 `psql -v ON_ERROR_STOP=1 -f ...`，让任何错误立即停止。SQL 整体在一个事务中执行：
   - 旧 `model_deploy` schema 不存在时，直接建两张表。
   - 旧五张表全部为空时，清理旧表及 schema，再建两张表；不使用 CASCADE。
   - 任意旧表有记录、出现额外表或未知依赖时，整笔迁移失败并回滚，原数据保留。此时保持模块关闭，先核实和迁移已有记录，不能直接删表重跑。
   - 新两表结构可重复执行迁移；不清理其中的部署历史，也不修改现有生成请求、实例或容量表。
3. 给现有 Gateway 运行账号授予以下权限。美东当前角色为 `model_gateway_exhibition`；其他环境替换实际角色名，不向 GPU 下发 DSN：

```sql
GRANT USAGE ON SCHEMA model_gateway TO model_gateway_exhibition;
GRANT SELECT, INSERT ON model_gateway.model_deployments,
  model_gateway.model_deployment_tasks TO model_gateway_exhibition;
GRANT UPDATE (phase, updated_at) ON model_gateway.model_deployments
  TO model_gateway_exhibition;
GRANT UPDATE (execution_action, status, claim_token, attempt, heartbeat_at, result, events, updated_at)
  ON model_gateway.model_deployment_tasks TO model_gateway_exhibition;
```

运行账号不需要建表、DELETE 或序列权限。部署定义由触发器保护；任务日志由管理 API 追加。旧 schema 的权限不会自动变成新表权限，必须执行以上授权。

4. 发布两表版本的 Gateway，再启用美东配置：**AWS us-east-2（Ohio）** 的 AppConfig `modelGateway / ppe_exhibition / gateway` 设置 `model_deployments_enabled: true`，并发布配置版本使其生效。G7e 本机在 us-east-1，不要改错配置区域；其他环境保持关闭。
5. 用管理凭据请求 `GET /v1/admin/deployments-ready`，应返回 `{"ready":true}`。这只验证部署存储可读；随后用实际部署任务验证写入、领取和回执。原 `/_gateway/ready` 仍表示生成账本就绪。

配置开关支持现有热更新。若回退到五表版代码，必须保持部署模块关闭；旧代码不能操作新两表结构。生成链路继续使用原有表和配置。数据库操作仍走仓库现有运维流程。

## 执行主机和清单

执行器需要 Linux、Python 3.11+、Docker Compose v2、NVIDIA Container Toolkit，以及同机独占的 `/var/lock/vmind-compose-release.lock`。安装所选模型输出验收插件的依赖；例如图片插件需要 VMind-Image `deploy/model_deploy/requirements.txt`。本版沿用预先下载的固定镜像与模型缓存，执行时检查摘要，不在 GPU 发布中临时安装模型依赖或构建镜像。

- `--credentials /secure/gateway.env`：0600 文件，含 `MODEL_GATEWAY_URL`、`MODEL_GATEWAY_ADMIN_TOKEN`。
- `MODEL_GATEWAY_API_TOKEN`：目标主机通过受保护环境注入，仅用于真实生成验收。
- 不使用 `MODEL_DEPLOY_DATABASE_URL`。数据库只由 Gateway 连接。
- 从模型仓库复制模板到工作目录，填写真实版本、云端 source、文件 SHA256、机器/GPU UUID、只读权重路径、独立 state_root 及验收输入。
- `version.output_validator` 指向一个 `role=validator` 的资产名称。执行器检查插件 SHA256 后，用自己的 Python 解释器调用 `--media <临时输出文件> --case <验收配置文件>`；插件非零退出即验收失败，成功须输出 JSON 对象。插件与模型版本一起固定，不能加载未校验的本地代码。
- 模板和代码上 Git；具体部署 manifest、Compose 和任务回执以 DB 为准。凭据使用运行时环境/文件引用，不写入 manifest、版本或 source URL。

通用模型必须把 `${MODEL_ID}` 与 `${VALIDATION_MODEL_ID}` 绑定到同一 profile/能力。候选用独立验收键，先核对真实实例、权重、PE 条件和输出，再开放正式入口。原生业务模型继续使用业务协议，需要现有 native_backends 配置覆盖临时键；地址/端口遵守既有 Gateway 后端限制。注册/探活/共享资源组/drain 功能直接复用。

## 两个部署场景

在提交机准备清单和 Compose 后提交；该步骤只写入任务，不在 Gateway 服务端执行 Docker。显式 task_id 用来处理网络超时：不确定是否提交成功时先查询原 ID，不换新 ID。

```sh
# 新 model_id，提交后尚未执行。
python deploy/model_deploy/manage.py --credentials /secure/gateway.env \
  deploy /work/models/new/manifest.json --activate --task-id body-new-001

# 在清单指定的 GPU 主机执行同一个任务。必须注入 API Token。
python deploy/model_deploy/manage.py --credentials /secure/gateway.env execute body-new-001

# 保持 model_id 更新 LoRA；前驱由 Gateway DB 导出，无需旧机器上的清单作为事实来源。
python deploy/model_deploy/manage.py --credentials /secure/gateway.env \
  switch-lora /work/models/next/manifest.json --from body-v1-gpu0 --task-id body-switch-002
python deploy/model_deploy/manage.py --credentials /secure/gateway.env execute body-switch-002
```

不带 `--activate` 的 deploy 只启动并暂停登记；之后使用新 task_id、同一份定义提交 `deploy --activate` 再执行。相同 deployment_id 不得换定义，不允许复用已经停机的实例身份。

执行前可在目标主机运行 `plan <manifest> [--from <deployment_id>]` 做只读文件、镜像、GPU、端口和登记检查。plan 不预留资源；真正提交和执行时仍重新校验。

同机空闲卡：启动候选 → 隔离验收 → 排空旧实例 → 开放新实例 → 停旧容器。
复用原卡：排空旧实例 → 停旧容器 → 核实资源释放 → 启动/验收候选 → 开放新实例。
失败：暂停候选并确认执行终态 → 保留状态、停止候选 → 恢复旧容器及入口。不能证明候选空闲时保留未完成任务的资源占用，禁止强杀或直接重开旧模型。

暂停旧实例和启用新实例是两个管理操作，存在排队间隔，不宣称原子切换。未绑定实例的请求可在新版本开放后执行；已绑定旧实例的重试保留原执行身份，发现此类请求会拒绝停机。`require_pe=true` 只影响候选验收，生产原有 PE 回退策略保持不变。

## 查询、回退和执行器中断

```sh
python deploy/model_deploy/manage.py --credentials /secure/gateway.env task body-switch-002
python deploy/model_deploy/manage.py --credentials /secure/gateway.env history body-v2-gpu0
python deploy/model_deploy/manage.py --credentials /secure/gateway.env export body-v2-gpu0 --output /work/recovered

# 显式回退也是独立任务，随后在原目标主机执行。
python deploy/model_deploy/manage.py --credentials /secure/gateway.env rollback body-v2-gpu0 --task-id body-rollback-003
python deploy/model_deploy/manage.py --credentials /secure/gateway.env execute body-rollback-003

# 仅用于 running/recovery_required 的中断任务：先确认原执行器已退出，再在原主机恢复。
python deploy/model_deploy/manage.py --credentials /secure/gateway.env execute body-switch-002 --recover
```

任务状态为 queued、running、succeeded、failed、recovery_required。定义的 phase 是快照；history/status 不能替代现场 Docker、Worker、Gateway 状态检查。

- 提交事务检查所有 queued/running/recovery_required 任务涉及的模型、候选 GPU 和前驱 GPU；冲突就拒绝。任务状态持久保存，任务 ID 重试不重复创建。
- 领取只允许清单指定的 machine_id/hostname；领取令牌绑定本次 attempt。查询接口不返回该令牌。
- 执行器每 15 秒心跳。心跳超时**不会**自动换执行器、重跑或释放资源占用。
- 回执包含固定 event_id，丢失响应后重试不会重复记账；过期领取令牌写回会被拒绝。
- `--recover` 必须先取得同机执行锁，确认原执行器已退出；然后换领取令牌。它只恢复旧版本（有前驱）或安全停止候选（无前驱），不继续执行上次部署步骤。管理端不能以直接调用 claim 代替这项现场检查。
- Gateway/DB 中断时，执行器不能把未确认的回执当成成功；未完成任务仍保留资源占用。恢复连接后先查询 task/history，再按上述流程恢复。
- 正常完成或已安全停机/回退的失败结束任务，才允许后续操作；未知执行保留未完成状态和资源占用。已有独立实例可用 `adopt <manifest> --task-id ...` 纳管，要求配置完全匹配并通过真实验收。

CLI 的 stop/rollback 返回 queued 只表示受理；必须 execute 后看到 succeeded 才算动作完成。旧版本、状态目录和历史不自动清理。

## 管理 API

所有以下接口仅接受现有管理凭据，不向普通 API/实验 Token 开放。当前执行器属于可信运维进程，使用管理凭据；该凭据具有既有实例管理权限，不宣称已实现按主机划分的代理权限。

| 方法 / 路径 | 用途 |
| --- | --- |
| GET `/v1/admin/deployments-ready` | 部署存储就绪 |
| POST `/v1/admin/deployment-tasks` | 原子提交不可变定义和操作任务 |
| GET `/v1/admin/deployment-tasks/:id` | 查询操作状态 |
| POST `/v1/admin/deployment-tasks/:id/claim` | 指定机器领取；显式 recover 仅由取得本机锁的恢复执行器调用 |
| POST `/v1/admin/deployment-tasks/:id/heartbeat` | 携领取令牌报告存活 |
| POST `/v1/admin/deployment-tasks/:id/events` | 携领取令牌和 event_id 追加回执 |
| POST `/v1/admin/deployment-tasks/:id/finish` | 完成任务，安全终态才允许后续操作 |
| GET `/v1/admin/deployments/:id` | 导出定义、Compose 和部署历史 |

## 验证

```sh
python -m unittest discover -s deploy/model_deploy -p 'test*.py'
TEST_DEPLOY_DATABASE_URL=<独立_test库> go test -race ./internal/deployment
# 与上一套数据库测试顺序执行；测试会重建两张部署表并验证旧 schema 清理。
TEST_DEPLOY_HTTP_DATABASE_URL=<独立_test库> go test -race ./internal/httpapi -run TestDeploymentAPI
```

未配置测试 DSN 时数据库用例会跳过，不能算验证通过。执行器状态机用模拟 GPU/Docker；真实美东验收必须另行验证新 model_id、稳定 ID LoRA 切换和失败回退，当前文档不代表这三个场景已经通过。
