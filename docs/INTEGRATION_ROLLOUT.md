# 首轮统一入口与通用生成整合

本轮基线是 modelGateway PR #7 的数据库 Schema/统一受理，合入 `feat/generic-model-generation`；调用方沿用 wgModelHub PR #30。Image 合入同名通用分支并带入 PR #10 的多图验收工具，Video 保留 PR #5 的业务验收工具。#10/#5 已关闭但未合并，不能把关闭状态当作已发布。

## 契约与职责

| 层 | 本轮职责与保留行为 |
|---|---|
| 业务调用方 / ModelHub | 保留 protobuf Generate 图片、Submit/Get 视频；把业务字段转换为 Gateway 原生 JSON；不新增业务流程、提示词或参数范围的重复校验。ModelHub 仍校验协议结构并归一化错误。 |
| Gateway 业务入口 | `/v1/gateway/requests/{model_key}` 按数据库 Schema 校验、解码媒体、鉴权、固定元数据、入账；图片同步等结果，视频 202；不恢复 text/model_traits 旧信封。 |
| Gateway 通用入口 | `/v1/images/generations`、`/v1/images/edits`、`/v1/videos` 仅做传输适配，同样使用数据库 Schema 与共用受理函数；普通 API 凭据按模型公开 Schema 调用，业务模型的实验参数权限保持原逻辑。 |
| Gateway 调度 | 共享原请求账本、UUID 幂等、RabbitMQ、物理 GPU 容量和结果存储。未知执行结果的重试保持实例/地址/制品归属。 |
| Image 模型服务 | 校验已加载 profile、真实图片和运行能力；业务入口保留默认参数、PE、透明处理与回执；通用入口按用户 prompt/已登记参数生成。 |
| 部署工具 | 同一绑定生成 Worker 配置与 Schema，完成探活、资源组核对和实例登记；不猜测拓扑、不自动扩大容量、不替换运行中的 Worker。 |

通用接口使用新 model_key / instance_id，资源组仍绑定实际同一物理卡。`input_schema` 属于模型，副本继承定义，显式冲突拒绝。Qwen/FLUX 首轮 n=1，seed 为 [0, 2^63)，具体参数见 Image 输出的定义。完整请求示例见 [通用 API](GENERATION_API.md) 与 README 的业务契约。

不在本轮新增：租户请求隔离、独立风控服务、模型版本注册中心、自动滚动发布控制器、ModelHub 图片跨调用幂等、VMind-Video 通用 Worker。现有 request_read 凭据权限继续保留，不能对外宣称已经按调用方隔离。

## 合并和发布顺序

1. **固定候选版本并审核差异。** Gateway 的统一入口和通用分支应作为一个整合候选，Image 同理；Hub 基于 #30 增补联调测试；Video 基于 #5 补齐下载验收。不得分别部署互相覆盖的 Gateway 分支。Gateway/Hub 的 dev push 会触发发布，先保持在 feature/draft PR 上审查。
2. **准备目标环境。** 核对实际 Gateway 地址、凭据引用、网络白名单、S3/OSS、PG/RabbitMQ、GPU 资源组和持久状态目录。保留当前配置、镜像 digest 和实例清单。Schema 变更或队列升级遵守现有排空规则，禁止拿测试脚本操作生产库。
3. **升级 Gateway。** 按 `deploy/migrations/model-input-schema.sql` 完成 Schema 字段和原业务种子回填，检查缺定义模型；先升级全部 Gateway/Worker，再登记通用模型。该脚本不会回填 image_generation/video_generation，历史通用模型缺定义须从实际部署绑定生成并用管理接口补齐。无需新增其他数据库字段或第二套队列。
4. **部署 Image 候选。** 从已审查源码构建固定制品，保留原业务键和持久幂等目录。同一 GPU 的业务/通用实例必须全部纳入该物理卡资源组。调整同卡驻留 Worker 前，排空该卡所有相关实例并确认无在途任务，不能只排空一个模型别名。先单卡验证再扩展。
5. **登记和核对目录。** 使用 Image `render.py --model-definitions` 生成定义，再以 `register.py ensure --model-definitions` 登记新通用模型；核对 `/v1/models/{model_key}`、模型专属 health_url、resource_group 和实际 artifact。Schema default 不会改写请求。变更绑定/能力须同步更新定义，不能让旧新能力在同一键下混跑。
6. **切换 ModelHub。** 同环境 Gateway 已就绪后，先更新 #30 要求的 `vision_gateway` 等配置，再发布 Hub。连接资源变化按既有规则滚动重启。ModelHub 业务图像仍 Generate，业务视频仍 Submit/Get；产品/算法直接使用通用 Gateway，本轮不改 protobuf。
7. **执行 GPU 验收。** 业务体模、双图试衣、原业务视频完整下载各一例；通用 Qwen/FLUX 按实际绑定各一例，核对 prompt、参考图顺序、权重与回执。非法参数不得入账；同 UUID 重放不得重复推理；业务/通用同卡并发不得超过容量。记录 request_id、instance_id、artifact_digest 和结果。视频工具保留 motion_tips 和稳定 UUID，下载为空、截断、失败或超时返回非零并不覆盖已有成品。

2026-11-17 已按授权在 aws-g7e 排空 GPU 2 的四个图像实例、GPU 1 的视频实例，完成隔离候选的真实生成并恢复原服务。BodyModel 使用 `originalfine-20261113`；业务/通用、multipart、RGBA、幂等、非法参数不入账和共享容量通过。随后修复服装 PE 配置/请求边界并补齐商品图、拆衣、明确衣物位置试衣各两次增强成功验收；发布时需同步部署版本化提示词配置与镜像。详细版本、证据以及生产存储/迁移等未覆盖项见 [GPU 验收记录](GPU_ACCEPTANCE_20261117.md)。该验收未升级生产 Gateway，也未覆盖远端工作区。正式发布继续遵循上述顺序。

## 可复现测试

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

先停止新通用调用并排空相关实例，保留幂等目录和历史任务执行归属。未知结果任务仍查询原实例，不为回退强行释放 GPU 槽。删除/清理实例必须满足现有未就绪且无占槽规则。

Gateway 回退版本若不认识通用协议，必须先处理完所有通用任务；保留 input_schema 数据，不执行破坏性删列。Hub 回退应与配置版本一起恢复，不能让旧二进制读取已删除的直连配置。Worker 使用固定旧制品与对应绑定恢复，再探活并恢复接单。
