# 首轮整合 GPU 验收（2026-11-17）

在 aws-g7e 完成候选 Gateway → 真实 GPU 的接口、回执和调度验收。BodyModel 使用用户指定的 `originalfine-20261113`。测试后恢复原图像容器与实例接单，GPU 0–3 的相关 10 个生产登记均 healthy/eligible，候选容器已移除。未合并或发布生产 Gateway。

## 固定版本与环境

- Gateway：`8c9fbfc50b88037ca1e172222ce69db43e2f7a7b`（PR #9）。
- Image：`d3f8ffa2ca7db307da60815795237018349f9e2e`（VMind-Image PR #11）。
- Video 验收工具：`58f06a8dc588e8510b1a1a5b9de91551d3c7b6a8`（VMind-Video PR #6）；推理使用当前 GPU 1 Worker。
- BodyModel：`WGDL/Qwen-Image-2.1-BodyModel-OriginalFine-SFT-20261113@74f1ac52208bccea564520f8b654128061dfb652`，权重 SHA256 `7af4773e7ebb5f73a2f484bfb22f87c7234cb834300130b99bac9293e7e91d3a`。
- 图像占用 GPU 2；该卡四个业务实例一起排空，GPU 3 副本继续服务。视频只排空 GPU 1 登记，未停止共享 GPU 0/1 的视频容器。
- 独立 Gateway、PostgreSQL 18 测试库、RabbitMQ 4 与本机文件型对象存储测试服务。所有候选端口仅监听 loopback。

Image 从当前固定基础镜像叠加候选源码构建。首次启动暴露基础镜像缺少 CuPy；补齐仓库已声明的 `cupy-cuda12x==13.6.0` 后启动与预热通过，未关闭透明后处理。模型权重只读挂载，候选状态、重放目录与归档独立；未改写远端脏工作区。

## 已通过

| 请求 | 真实结果 |
|---|---|
| 业务 BodyModel | 1024×1536 RGB，回执为 originalfine |
| 业务双图试衣 | 1024×1536 RGB，回执为 VTON AdaptiveCaption |
| 业务商品图、人物拆衣 | 各 1024×1024 RGB，回执为 FLUX VTOFF |
| 通用 BodyModel、双图试衣 | 各 512×768 RGB，分别选择 BodyModel/VTON 权重 |
| 通用 FLUX | 512×512 RGB |
| 通用 BodyModel RGBA | 512×768 RGBA，透明后处理开启 |
| OpenAI multipart 双图编辑 | 512×768 RGB，保留调用方 prompt，回执为 VTON 权重 |
| 业务视频提交、轮询、下载 | H.264、704×1280、24 fps、97 帧、4.041667 秒；ffmpeg 完整解码通过 |

图像均由 Pillow 完整解码并保存尺寸、模式、长度、SHA256。前八个图像用例以相同 UUID 重放，结果相同且 Worker 推理计数未增加。另直接重放 Worker：首请求只执行一次，同 UUID 同输入返回原结果，变更 prompt 返回 409。

普通业务凭据提交 seed、Qwen steps=4、显式 n=null、超过 int64 的 seed 均返回 400；管理查询对应 UUID 为 404，Worker 计数不变。业务 BodyModel 与通用 FLUX 并发时观测到 processing/queued，50 次采样中同卡最大占槽为 1，两请求最终成功。

请求 ID、实例、镜像 ID、权重回执和结果摘要见 [机器可读证据](evidence/gpu-acceptance-20261117.json)。只保存测试素材结果摘要及提示词哈希，不含凭据或业务图片。

## 验收边界与发布前剩余项

- **PE 成功路径已补齐。** 首轮观测到的回退及本次修复、成功路径验收见下节；普通试衣提示词仍保留既有 legacy 增强兼容路径。
- **生产 S3、生产数据库迁移尚未验收。** 本次对象存储是支持 S3 调用的本地测试服务，不能替代真实存储权限、配置及迁移演练。
- **生产 ModelHub → GPU 尚未联调。** 已有真实 ModelHub HTTP 适配器/Schema 契约测试，但本次 GPU 请求由验收工具发起。
- **尚未执行正式镜像构建与发布。** 需按正式 Dockerfile 构建、固定 registry digest，并依据 [发布顺序](INTEGRATION_ROLLOUT.md) 做目标环境迁移及滚动发布；不直接把本次源码叠加镜像当作正式制品。
- CUDA 后处理实际生成路径已执行；独立 CUDA 单元测试套件不因本次验收自动记为通过。

四个整合 PR 仍保持 draft。候选 GPU 验收已完成，后续阶段是正式制品与目标环境发布验证，无需重复设计统一入口、Schema 发布或共享容量能力。

## PE 回退修复与增强成功验收

**商品图、人物拆衣：问题—措施—效果。** 首轮虽然出图成功，实际没有应用预期的服装特征增强。原因一是 v3 配置引用了旧提示词文件，历史镜像中的手工覆盖被源码更新还原；原因二是完整业务提示词混入特征提取请求，导致 PE 输出整段生成文案而非四个特征字段。部署改为显式引用 v3 文件，镜像构建及忽略规则同步打包提示词；代码只向 PE 发送图片、目标信息与特征提取要求，随后由服务端拼装并保留业务及自定义约束。严格校验、长度限制和失败回退未放宽。

只修配置时，同一靴子用例两次仍回退；补齐代码后，商品图、人物拆衣各两次均成功应用 PE，无回退原因、无 PE 重试。成功重试还会清除旧的顶层回退原因，尝试历史仍保留。两个商品图样本的 PE 阶段耗时从约 4.90–5.10 秒降至 1.73 秒；这只是本次功能样本，不代表整体性能。

**试衣：问题—措施—效果。** 原普通提示词没有明确衣物位置，触发 `no_explicit_grid_inventory` 并调用既有 legacy PE；不是增强服务不可用。保留该兼容行为，用与原图一致的一行两列外套/短裤清单补测 compact PE，两次均 applied。未改动 VTON 增强路由。

| 验收项 | 结果 |
|---|---|
| 商品图 PE | 2/2 applied，1024×1024，完整解码 |
| 人物拆衣 PE | 2/2 applied，1024×1024，完整解码 |
| 明确衣物位置的试衣 PE | 2/2 applied，1024×1536，完整解码 |
| Image 业务 CPU / 登记验收工具 | 146 / 14 项通过 |
| 提示词打包 | 使用正式 Dockerfile 的 ignore 规则，COPY 构建检查通过 |
| 恢复 | 原四个容器镜像和环境一致，10 个生产登记健康可接单，候选容器清理 |

验收工具新增 `--require-pe`：即使生成成功，fallback、disabled 或缺少状态也返回失败。代码修复版本为 Image `c96e5cc4334f5896b65fc5e949eeb742f5c452d0`；Gateway 运行版本保持首轮版本。详细请求、输入/输出摘要、固定制品与恢复证据见 [PE 机器可读记录](evidence/pe-acceptance-20261117.json)。原始输入及新旧完整输出保存在授权验收目录，未将业务图片提交到公开仓库。

对比限制：旧版样本已经修正提示词路径，但尚未修复代码；新旧随机种子不同，图片只能作为功能验收证据，不能推导画质整体提升。使用隔离 Gateway 与本地对象存储测试服务，生产 S3、迁移、Hub 链路及正式制品发布仍按上节待完成项执行。

## 正式构建与 ModelHub 链路验收（2026-11-17）

已使用仓库正式 Dockerfile 构建 Image Qwen、FLUX、入口服务，以及 Gateway、ModelHub、ModelHub migration 共六个制品，推送到现有 ECR 后通过 BatchGetImage 核对 digest。不是前述源码叠加镜像。版本为 Image `56ac908`、Gateway `6639cda`、Hub `87beb84`；Hub 已合入最新 dev，保留同期 ASR 和 embedding batch 改动。之后 Hub `c988980` 仅修正视觉配置 Schema、样例、测试和文档，未改变运行时 Go 代码；正式 GPU 验收记录对应 `87beb84`，不得把后续提交称为已重新构建的制品。

| 检查 | 结果 |
|---|---|
| 正式 Image 镜像 CPU 回归 | 146 个业务测试、22 个 Qwen server/generic 测试通过；业务测试挂载源码 configs 测试夹具，未覆盖运行时代码 |
| 正式 Hub 构建测试 | production / migration Dockerfile 的完整 Go 测试和 genai patch 测试通过 |
| Hub gRPC → Gateway → GPU → 结果存储 → Hub 图片流 | BodyModel、商品图、拆衣、试衣四例完整 PNG 解码通过；后三例 PE applied；体模按默认禁用 PE |
| Hub Submit/Get 视频 | 完整 H.264 视频可解码，704×1280、97 帧；Hub 重启后相同 request_id 复用同一任务与结果，Gateway 只有一条该视频请求 |
| 通用生成 | BodyModel、Outfit、Wardrobe 三例通过，保留调用方 prompt、PE disabled、seed；同 UUID 重放无重复 Worker 推理 |
| 恢复 | 原四个容器镜像及环境一致，10 个生产实例 healthy/eligible；未升级生产 Gateway 或 ModelHub |

原始请求 ID、输出 SHA256、重放、制品和恢复事实见 [正式验收证据](evidence/formal-release-acceptance-20261117.json)；远端仓库的六个 digest 核对见 [ECR 验证](evidence/registry-verification-20261117.json)。证据只含摘要，不含 Token、配置凭据和原始业务媒体。

**仍未通过的发布门槛：** 本轮 Hub 是正式镜像启动的隔离完整服务，PG/MQ 与 S3 仍为隔离测试设施，不能宣称生产 ModelHub、生产 S3 权限或生产数据库迁移已验收。真实 AppConfig 已读到；五个视觉 provider 的旧直连字段会导致候选 Hub 启动失败，另有国内 L20-1 `human-parser` 尚未接入 Gateway。需先完成配置迁移及国内解析转发，再验证完整生产配置。当前本机 IAM 对结果桶 HeadBucket 返回 403；它不能证明 ECS TaskRole 缺权限，也不能替代以服务运行身份完成存储验收。G7e 到生产 Hub 内网 gRPC 端口不可达，需要已有授权运行入口。所有 PR 保持草稿，不据此合并触发生产部署。
