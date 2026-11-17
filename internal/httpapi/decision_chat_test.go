package httpapi

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/internal/inputschema"
)

func TestDecisionChatPreservesPresetAndInlineImage(t *testing.T) {
	fields := map[string]json.RawMessage{
		"messages":     json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`),
		"model_traits": json.RawMessage(`{"task":"image_type"}`), "stream": json.RawMessage(`true`),
	}
	schema, err := inputschema.Seed("decision")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(fields map[string]json.RawMessage) (map[string]any, error) {
		raw, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		return inputschema.Validate(schema, raw, false)
	}
	parsed, err := validate(fields)
	if err != nil {
		t.Fatal(err)
	}
	body, contentType, stream, err := prepareDecisionChat(parsed)
	if err != nil || !stream {
		t.Fatalf("prepare Decision: %v", err)
	}
	_, params, _ := mime.ParseMediaType(contentType)
	reader := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	form, err := reader.ReadForm(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()
	if form.Value["task"][0] != "image_type" || len(form.Value["prompt"]) != 0 || len(form.File["image"]) != 1 || form.File["image"][0].Filename != "image.png" {
		t.Fatalf("native form changed: %+v", form)
	}
	file, _ := form.File["image"][0].Open()
	raw, _ := io.ReadAll(file)
	file.Close()
	if string(raw) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("image bytes changed")
	}
	for _, tc := range []struct{ name, messages, traits string }{
		{"remote image", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://private/image.png"}}]}]`, `{"task":"image_type"}`},
		{"missing image", `[{"role":"user","content":"classify"}]`, `{}`},
		{"invalid image", `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,dGV4dA=="}}]}]`, `{"task":"image_type"}`},
		{"mixed task and prompt", `[{"role":"user","content":[{"type":"text","text":"classify"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`, `{"task":"image_type"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields["messages"], fields["model_traits"] = json.RawMessage(tc.messages), json.RawMessage(tc.traits)
			parsed, err := validate(fields)
			if err == nil {
				_, _, _, err = prepareDecisionChat(parsed)
			}
			if err == nil {
				t.Fatal("invalid Decision input was accepted")
			}
		})
	}
}

func TestDecisionChatPreservesNullMessageSpacing(t *testing.T) {
	schema, err := inputschema.Seed("decision")
	if err != nil {
		t.Fatal(err)
	}
	fields, err := inputschema.Validate(schema, json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]},{"role":"user","content":"a"},{"role":"user","content":null},{"role":"user","content":"b"}],"model_traits":{"options":["shirt","pants"]}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	body, contentType, _, err := prepareDecisionChat(fields)
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(strings.NewReader(string(body)), params["boundary"]).ReadForm(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()
	if form.Value["prompt"][0] != "a\n\nb" || form.Value["options"][0] != `["shirt","pants"]` || len(form.Value["task"]) != 0 {
		t.Fatalf("custom question changed: %+v", form.Value)
	}
}
