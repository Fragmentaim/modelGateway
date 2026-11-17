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

- **PE 成功路径未覆盖。** 商品图/拆衣触发既有 `PE feature fields are missing, duplicated or invalid` 回退；试衣触发 `no_explicit_grid_inventory` 回退。生成成功和回退回执已验证，不能据此宣称 PE 增强成功。
- **生产 S3、生产数据库迁移尚未验收。** 本次对象存储是支持 S3 调用的本地测试服务，不能替代真实存储权限、配置及迁移演练。
- **生产 ModelHub → GPU 尚未联调。** 已有真实 ModelHub HTTP 适配器/Schema 契约测试，但本次 GPU 请求由验收工具发起。
- **尚未执行正式镜像构建与发布。** 需按正式 Dockerfile 构建、固定 registry digest，并依据 [发布顺序](INTEGRATION_ROLLOUT.md) 做目标环境迁移及滚动发布；不直接把本次源码叠加镜像当作正式制品。
- CUDA 后处理实际生成路径已执行；独立 CUDA 单元测试套件不因本次验收自动记为通过。

四个整合 PR 仍保持 draft。候选 GPU 验收已完成，后续阶段是正式制品与目标环境发布验证，无需重复设计统一入口、Schema 发布或共享容量能力。
