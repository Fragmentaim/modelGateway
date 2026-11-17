# 美东生成接口整合发布

本次发布使用 Gateway 与 ModelHub 的 `codex/generation-rollout`。它承接已验证的 Schema/通用生成能力，并将视觉强制迁移从本批中拆出。原整合草稿包含视觉配置删除，不能直接作为本批 ModelHub 发布候选。国内 human-parser、独立风控和部署控制器不属于本批。

## 已上线与待发布

- 已上线：G7e GPU2/3 的 FLUX 服装生成服务更新 PE 调用代码和版本化提示词配置。**PE 推理服务仍在 GPU0，未迁移或重启。** 生产 Gateway → GPU → S3 写入/取回及四次 PE applied 已通过，详细证据在 Image 的 `deploy/qwen_pe/production-pe-20261117.json`。
- 待发布：Gateway 输入 Schema、模型目录、OpenAI-compatible 通用生成入口，以及 ModelHub 的规范业务 JSON 映射。
- 待完成的生产门槛：现有 ModelHub gRPC 全链路验收。一次性 ECS 客户端已能启动，但从 ModelHub 自身安全组访问 50053 超时；正在确认既有业务方的网络入口。当前没有以该验收为由改生产服务或安全组。

## 保持的契约

| 层 | 本批行为 |
|---|---|
| ModelHub | protobuf 不变；图片 Generate、视频 Submit/Get 不变。图像适配器从 Gateway 读取协议，把 text/traits 展开成 garment_name、garment_role、bbox、parse_labels、prompt、gender 等规范字段。 |
| Gateway 业务入口 | 兼容生产 ModelHub 的旧图片信封和新字段；四种业务图片协议先规范化，再按数据库 Schema 校验。禁止混合新旧字段；相同 UUID、相同业务输入仍返回同一任务。 |
| Gateway 通用入口 | Images generations/edits 与 videos 共用 Schema、请求账本、幂等、资源组和存储；通用模型必须显式登记能力，不能借用业务模型键。 |
| 现有视觉/对话/决策 | 保留原固定入口、鉴权、上传限制、实例调度与追踪。ModelHub 本批保留现有视觉适配器和配置，包括国内 human-parser 直连。 |
| 模型服务 | 保留真实图片/运行能力校验及业务默认值。在线 PE 失败仍可回退生成；“必须 PE applied”只作为修复验收要求。 |

## 合并及发布顺序

1. 固定两个新候选的提交和镜像 digest。feature PR 不触发 dev 自动发布；本批不要直接合并原 Hub 全量视觉整合草稿。
2. 完成现有生产 Hub 基线验收及候选的跨仓库测试。真实生产配置结构必须兼容；使用既有运行时身份读取生产 S3，不能以本地假存储代替。
3. 备份配置和镜像，按既有排空规则核实在途任务。执行 `deploy/migrations/model-input-schema.sql`，只补列和首次回填，保留自定义 Schema；检查所有已登记模型均有定义。不要用集成测试命令操作生产库。
4. 发布 Gateway。旧 Hub 的图片信封、旧视觉/对话入口继续工作。先通过现有业务链路再进入下一步；不同时增加通用模型流量。
5. 发布生成专用 ModelHub。沿用现有 AppConfig 和视觉连接配置，无需为本批删除直连字段或迁移国内解析。核对四种业务图片与业务视频返回，并确认 PE applied 路径。
6. 全部 Gateway 副本稳定后，才按实际已驻留 Worker 能力登记通用模型。使用 Image 既有 render/register 工具生成 Schema 和绑定，同一物理 GPU 共享同一资源组。不得仅改协议标签或虚构可用的模型。若需要替换 Worker，先排空该卡所有关联实例并单卡验收；不因发布 Gateway/Hub 自动替换 Qwen/Video。
7. 业务与通用各跑对应验收，记录请求、模型、实例、镜像和输出。非法参数不得入账；重放不得重复推理；共享 GPU 不超占。

## 验证

2026-11-17：本候选已通过 `go test -race ./...`（独立 PostgreSQL/RabbitMQ）、`scripts/ci/test-generic-contracts.sh`（真实 Python 路由/Schema 和 Hub 适配器，CPU 推理替身）、`go vet ./...` 与 `go mod verify`。生产 Hub 验收仍受上述网络入口限制。

新增兼容测试覆盖旧固定入口实际派发、追踪与重复提交保护，以及四种旧图片信封与规范 JSON 使用同 UUID 重放同一任务。ModelHub 保留原视觉配置和媒体传输测试，并验证规范字段映射与动态模型键。历史隔离 GPU/正式镜像验收见 `GPU_ACCEPTANCE_20261117.md`，不代表本批新候选已经部署生产。

常规 Gateway 测试使用独立 `_test` PostgreSQL 和 RabbitMQ：

```sh
go test -race ./... -count=1
go vet ./...
go mod verify
```

测试前设置 `TEST_DATABASE_URL`、`TEST_RABBITMQ_URL`。跨仓库发布检查还必须设置以下环境变量，不能仅依赖默认会 Skip 的可选测试：

- `VMIND_IMAGE_TEST_URL`：启动 `VMind-Image/tests/generic_gateway_fixture.py <url-file>` 后写入文件的 URL；需要 Pillow、FastAPI、httpx、aiohttp、uvicorn、numpy。
- `VMIND_IMAGE_MODEL_DEFINITIONS`：同一 fixture 输出的 `<url-file>.models.json`。
- `MODELHUB_CONTRACT_DIR`：含 `gateway_contract_test.go` 的 Hub 候选检出绝对路径。

然后执行 `bash scripts/ci/test-generic-contracts.sh`。缺少变量、Schema 文件或 Hub 测试文件会失败。它验证真实 Python HTTP/路由及 Schema、真实 Hub 适配器与 Gateway HTTP/数据库；推理函数为 CPU 替身，存储为测试服务。测试共用数据库，不要并发启动多套 Go integration suite。

Image 另外运行 `run_business_api_cpu.py`、Qwen `test_server.py` / `test_generic_api.py`、共享入口路由、`deploy/unified/test*.py`、`deploy/model_gateway/test*.py`。Video 运行 `python -m unittest discover -s deploy -p test_accept_model_gateway.py`。Hub 运行配置及 vmindgateway/vworldimage/ltx/factory/modelhub service 的 race 测试。

## 回退

Hub 回退只需恢复上一镜像，因为本批未改视觉配置结构。旧 Hub 和新 Hub 都能通过本批 Gateway 的兼容入口调用。Gateway 回退前，先停止通用入口新增调用、排空所有新通用任务并核实未知执行归属；旧 Gateway 不认识通用协议。保留 input_schema 列及数据，不作破坏性回退。PE 调用修复与这两项升级独立，正常回退 Gateway/Hub 不需要回退已上线 FLUX 镜像。
