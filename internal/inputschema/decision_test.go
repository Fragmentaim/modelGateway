package inputschema

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestDecisionSchemaOwnsBusinessRules(t *testing.T) {
	schema, err := Seed("decision")
	if err != nil {
		t.Fatal(err)
	}
	const image = `{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}`
	imageMessage := `{"role":"user","content":[` + image + `]}`
	for _, tc := range []struct {
		name, messages, traits string
		valid                  bool
	}{
		{"preset", imageMessage, `{"task":" image_type "}`, true},
		{"preset empty options", imageMessage, `{"task":"image_type","options":[]}`, true},
		{"custom string", imageMessage + `,{"role":"user","content":" classify "}`, `{}`, true},
		{"custom parts", `{"role":"user","content":[` + image + `,{"type":"text","text":" classify "}]}`, `{"options":["a","b"]}`, true},
		{"blank task and custom prompt", imageMessage + `,{"role":"user","content":"classify"}`, `{"task":" \t "}`, true},
		{"blank user text with preset", imageMessage + `,{"role":"user","content":"\u0085\u00a0\u3000"}`, `{"task":"image_type"}`, true},
		{"system text is not prompt", imageMessage + `,{"role":"system","content":"instruction"}`, `{"task":"image_type"}`, true},
		{"two images in one message", `{"role":"user","content":[` + image + `,` + image + `]}`, `{"task":"image_type"}`, false},
		{"two images across messages", imageMessage + `,` + imageMessage, `{"task":"image_type"}`, false},
		{"missing image", `{"role":"user","content":"classify"}`, `{}`, false},
		{"missing content", imageMessage + `,{"role":"assistant"}`, `{"task":"image_type"}`, false},
		{"missing task and prompt", imageMessage, `{}`, false},
		{"whitespace task and prompt", imageMessage + `,{"role":"user","content":"\u00a0\u3000"}`, `{"task":" \t "}`, false},
		{"mixed task and prompt", imageMessage + `,{"role":"user","content":"classify"}`, `{"task":"image_type"}`, false},
		{"preset options", imageMessage, `{"task":"image_type","options":["a"]}`, false},
		{"unsupported part", `{"role":"user","content":[` + image + `,{"type":"audio"}]}`, `{"task":"image_type"}`, false},
		{"invalid text type", `{"role":"user","content":[` + image + `,{"type":"text","text":1}]}`, `{"task":"image_type"}`, false},
		{"remote image", `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://private/image.png"}}]}`, `{"task":"image_type"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := json.RawMessage(fmt.Sprintf(`{"messages":[%s],"model_traits":%s}`, tc.messages, tc.traits))
			if err := Validate(schema, input, false); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
