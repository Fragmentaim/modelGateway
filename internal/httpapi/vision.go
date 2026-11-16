package httpapi

import "github.com/wgdl666/modelGateway/internal/protocol"

type visionRoute struct {
	path, mediaType    string
	modelKey, protocol string
}

// 原生执行端的路径与编码；对外请求统一由模型 Schema 校验。
var visionRoutes = []visionRoute{
	{
		path: "/human-parser/human_parser/predict", modelKey: protocol.HumanParseModel, protocol: protocol.HumanParseProtocol,
		mediaType: "application/json",
	},
	{
		path: "/human-yolo/predict", modelKey: protocol.HumanYOLOModel, protocol: protocol.HumanYOLOProtocol,
		mediaType: "multipart/form-data",
	},
	{
		path: "/facesim/compare", modelKey: protocol.FaceSimModel, protocol: protocol.FaceSimProtocol,
		mediaType: "multipart/form-data",
	},
	{
		path: "/segment-person/api/segment", modelKey: protocol.SegmentPersonModel, protocol: protocol.SegmentPersonProtocol,
		mediaType: "multipart/form-data",
	},
	{
		path: "/segment-person/api/segment_subject", modelKey: protocol.SegmentSubjectModel, protocol: protocol.SegmentSubjectProtocol,
		mediaType: "multipart/form-data",
	},
	{
		path: "/lens_distortion_correction/correct", modelKey: protocol.LensCorrectionModel, protocol: protocol.LensCorrectionProtocol,
		mediaType: "application/json",
	},
}
