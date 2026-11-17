package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"strings"
)

// prepareLegacyDecisionChat 只转换已登记的 Decision 输入，保留原生预设任务/自定义问题互斥和内联图片边界。
func prepareLegacyDecisionChat(fields map[string]json.RawMessage) ([]byte, string, bool, error) {
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(fields["messages"], &messages) != nil {
		return nil, "", false, errors.New("Decision requires messages")
	}
	var images []string
	var texts []string
	for _, message := range messages {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			if message.Role == "user" {
				texts = append(texts, text)
			}
			continue
		}
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(message.Content, &parts) != nil {
			return nil, "", false, errors.New("invalid Decision content")
		}
		for _, part := range parts {
			switch part.Type {
			case "text":
				if message.Role == "user" {
					texts = append(texts, part.Text)
				}
			case "image_url":
				images = append(images, part.ImageURL.URL)
			default:
				return nil, "", false, errors.New("unsupported Decision content")
			}
		}
	}
	if len(images) != 1 {
		return nil, "", false, errors.New("Decision requires exactly one inline image")
	}
	prefix, encoded, ok := strings.Cut(images[0], ",")
	if !ok || !strings.HasPrefix(prefix, "data:image/") || !strings.HasSuffix(prefix, ";base64") {
		return nil, "", false, errors.New("Decision requires an inline image")
	}
	image, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "", false, errors.New("invalid Decision image")
	}
	// Decision 按扩展名读图片，必须从实际内容选择，不能信任调用方声明的 MIME。
	extension := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}[http.DetectContentType(image)]
	if extension == "" {
		return nil, "", false, errors.New("Decision requires a JPEG, PNG or WebP image")
	}
	var traits struct {
		Task    string   `json:"task"`
		Options []string `json:"options"`
	}
	if raw := fields["model_traits"]; len(raw) > 0 && json.Unmarshal(raw, &traits) != nil {
		return nil, "", false, errors.New("invalid Decision model_traits")
	}
	task, prompt := strings.TrimSpace(traits.Task), strings.TrimSpace(strings.Join(texts, "\n"))
	if (task == "") == (prompt == "") || (task != "" && len(traits.Options) > 0) {
		return nil, "", false, errors.New("Decision requires a preset task or a custom prompt with optional options")
	}
	var stream bool
	if raw := fields["stream"]; len(raw) > 0 && json.Unmarshal(raw, &stream) != nil {
		return nil, "", false, errors.New("stream must be a boolean")
	}
	var body bytes.Buffer
	out := multipart.NewWriter(&body)
	file, err := out.CreateFormFile("image", "image"+extension)
	if err != nil {
		return nil, "", false, err
	}
	if _, err = file.Write(image); err != nil {
		return nil, "", false, err
	}
	key, value := "task", task
	if prompt != "" {
		key, value = "prompt", prompt
	}
	if err = out.WriteField(key, value); err != nil {
		return nil, "", false, err
	}
	if len(traits.Options) > 0 {
		options, _ := json.Marshal(traits.Options)
		if err = out.WriteField("options", string(options)); err != nil {
			return nil, "", false, err
		}
	}
	if err = out.Close(); err != nil {
		return nil, "", false, err
	}
	return body.Bytes(), out.FormDataContentType(), stream, nil
}
