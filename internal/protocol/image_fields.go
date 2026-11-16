package protocol

import (
	"encoding/base64"
	"errors"
)

// DecodeImageFields 复用受理时已解析的 JSON 对象，只解码图片字节。
func DecodeImageFields(fields map[string]any) ([]InputImage, error) {
	items, ok := fields["images"].([]any)
	if !ok {
		return nil, errors.New("images must be an array")
	}
	images := make([]InputImage, 0, len(items))
	for _, item := range items {
		image, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("image must be an object")
		}
		// 与 Worker 的 typed JSON 解码保持一致，编码不支持的图片字段在入队前拒绝。
		for key := range image {
			if key != "mime_type" && key != "data" {
				return nil, errors.New("unsupported image field")
			}
		}
		mime, ok := image["mime_type"].(string)
		if !ok || !imageMIME.MatchString(mime) {
			return nil, errors.New("invalid image MIME type")
		}
		encoded, ok := image["data"].(string)
		if !ok {
			return nil, errors.New("image data must be base64")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 {
			return nil, errors.New("invalid image bytes")
		}
		images = append(images, InputImage{MIMEType: mime, Data: data})
	}
	return images, nil
}
