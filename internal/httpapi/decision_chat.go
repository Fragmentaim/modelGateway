package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

const decisionChatStreamKey = "decision_chat_stream"

// prepareDecisionChat 编码已通过模型 Schema 的输入；消息类型、图片数量和 task/prompt 互斥由 Schema 约束。
func prepareDecisionChat(fields map[string]any) ([]byte, string, bool, error) {
	messages, ok := fields["messages"].([]any)
	if !ok {
		return nil, "", false, errors.New("cannot decode Decision messages")
	}
	var imageURL string
	var texts []string
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			return nil, "", false, errors.New("cannot decode Decision message")
		}
		switch content := message["content"].(type) {
		case nil:
			if message["role"] == "user" {
				texts = append(texts, "")
			}
		case string:
			if message["role"] == "user" {
				texts = append(texts, content)
			}
		case []any:
			for _, value := range content {
				part, ok := value.(map[string]any)
				if !ok {
					return nil, "", false, errors.New("cannot decode Decision content part")
				}
				switch part["type"] {
				case "text":
					text, err := decisionString(part["text"])
					if err != nil {
						return nil, "", false, err
					}
					if message["role"] == "user" {
						texts = append(texts, text)
					}
				case "image_url":
					url, ok := part["image_url"].(map[string]any)
					if !ok {
						return nil, "", false, errors.New("cannot decode Decision image URL")
					}
					var err error
					imageURL, err = decisionString(url["url"])
					if err != nil {
						return nil, "", false, err
					}
				default:
					return nil, "", false, errors.New("cannot encode Decision content type")
				}
			}
		default:
			return nil, "", false, errors.New("cannot decode Decision content")
		}
	}
	_, encoded, _ := strings.Cut(imageURL, ",")
	image, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "", false, err
	}
	// Decision 按扩展名读图片，从实际字节选择格式。
	extension := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}[http.DetectContentType(image)]
	if extension == "" {
		return nil, "", false, errors.New("Decision requires a JPEG, PNG or WebP image")
	}
	var traits map[string]any
	if value := fields["model_traits"]; value != nil {
		var ok bool
		traits, ok = value.(map[string]any)
		if !ok {
			return nil, "", false, errors.New("cannot decode Decision model_traits")
		}
	}
	task, err := decisionString(traits["task"])
	if err != nil {
		return nil, "", false, err
	}
	task, prompt := strings.TrimSpace(task), strings.TrimSpace(strings.Join(texts, "\n"))
	var stream bool
	if value := fields["stream"]; value != nil {
		var ok bool
		stream, ok = value.(bool)
		if !ok {
			return nil, "", false, errors.New("cannot decode Decision stream")
		}
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
	if value := traits["options"]; value != nil {
		options, ok := value.([]any)
		if !ok {
			return nil, "", false, errors.New("cannot decode Decision options")
		}
		if len(options) > 0 {
			raw, err := json.Marshal(options)
			if err != nil {
				return nil, "", false, err
			}
			if err = out.WriteField("options", string(raw)); err != nil {
				return nil, "", false, err
			}
		}
	}
	if err = out.Close(); err != nil {
		return nil, "", false, err
	}
	return body.Bytes(), out.FormDataContentType(), stream, nil
}

func decisionString(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("cannot decode Decision string field")
	}
	return text, nil
}

func writeDecisionChat(c *app.RequestContext, body *synchronousBody, id, model string, stream bool) {
	defer body.Close()
	const maxResponse = 1 << 20
	// 先校验再结算；否则原生 200 的坏响应会在对外返回 502 前被记成成功。
	raw, err := io.ReadAll(io.LimitReader(body.ReadCloser, maxResponse+1))
	body.result.ResponseBytes += int64(len(raw))
	var result struct {
		Prediction struct {
			Label       string   `json:"label"`
			Probability *float64 `json:"probability"`
		} `json:"prediction"`
	}
	if err != nil || len(raw) > maxResponse || json.Unmarshal(raw, &result) != nil || result.Prediction.Label == "" ||
		result.Prediction.Probability == nil || *result.Prediction.Probability < 0 || *result.Prediction.Probability > 1 {
		body.result.StatusCode = http.StatusBadGateway
		if err != nil {
			body.finish.Do(func() {
				body.complete(body.result, false)
				_ = body.ReadCloser.Close()
			})
		}
		writeError(c, 502, "invalid_prediction", "invalid Decision prediction", id)
		return
	}
	messageKey, object := "message", "chat.completion"
	if stream {
		messageKey, object = "delta", "chat.completion.chunk"
	}
	response := map[string]any{
		"id": id, "model": model, "object": object,
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", messageKey: map[string]string{"role": "assistant", "content": string(raw)}}},
	}
	if stream {
		// Decision 原生只返回一次完整分类；流式契约仍是一条内容增量和结束标记。
		encoded, _ := json.Marshal(response)
		c.Data(200, "text/event-stream", append(append([]byte("data: "), encoded...), []byte("\n\ndata: [DONE]\n\n")...))
		return
	}
	writeJSON(c, 200, response)
}
