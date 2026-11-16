package inputschema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSeedsAndCredentialFields(t *testing.T) {
	entries, err := seeds.ReadDir("seeds")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := seeds.ReadFile("seeds/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckDefinition(raw); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
		}
	}
	schema, _ := Seed("vton")
	for _, tc := range []struct {
		input               string
		experimental, valid bool
	}{
		{`{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eA=="}],"prompt":"try on"}`, false, true},
		{`{"images":[{"mime_type":"image/png","data":"eA=="}]}`, false, false},
		{`{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eA=="}],"use_pe":"false"}`, false, false},
		{`{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eA=="}],"use_pe":"false"}`, true, true},
	} {
		if _, err := Validate(schema, json.RawMessage(tc.input), tc.experimental); (err == nil) != tc.valid {
			t.Errorf("valid=%v: %v", tc.valid, err)
		}
	}
}

func TestInputErrorsDescribeRulesWithoutEchoingValues(t *testing.T) {
	const marker = "PRIVATE_IMAGE_DATA_"
	value := strings.Repeat(marker, 1<<15)
	for _, rule := range []string{`{"type":"string","pattern":"^allowed$"}`, `{"enum":["allowed"]}`} {
		schema := json.RawMessage(`{"type":"object","properties":{"image":` + rule + `}}`)
		input, err := json.Marshal(map[string]string{"image": value})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Validate(schema, input, false)
		if err == nil {
			t.Fatal("invalid value accepted")
		}
		message := err.Error()
		if strings.Contains(message, marker) || len(message) > 256 || !strings.Contains(message, "/image") {
			t.Fatalf("unexpected schema error summary: %q", message)
		}
	}
}

func TestStringValidationDoesNotCopyMedia(t *testing.T) {
	schema, err := Compile(json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","pattern":"^data:image/"}`))
	if err != nil {
		t.Fatal(err)
	}
	value := "data:image/png;base64," + strings.Repeat("A", 1<<20)
	measurement := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := schema.Validate(value); err != nil {
				b.Fatal(err)
			}
		}
	})
	// 验证库不再为 1MiB 字符串分配同等大小的 []byte；这里不包含 JSON 解码。
	if bytes := measurement.AllocedBytesPerOp(); bytes >= 1<<20 {
		t.Fatalf("string validation copied media: %d bytes/op", bytes)
	}
}

func TestSchemaOwnsValidationAndRejectsRemoteRefs(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["thinking_level"],"properties":{"thinking_level":{"enum":["high"]}},"additionalProperties":false}`)
	if _, err := Validate(schema, json.RawMessage(`{"thinking_level":"high"}`), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(schema, json.RawMessage(`{"thinking_level":"low"}`), false); err == nil {
		t.Fatal("enum was not enforced")
	}
	if err := CheckDefinition(json.RawMessage(`{"$ref":"https://example.com/remote.json"}`)); err == nil {
		t.Fatal("remote reference accepted")
	}
	chat, _ := Seed("chat_completions")
	if _, err := Validate(chat, json.RawMessage(`{"messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"tool","content":"ok"}],"reasoning_effort":"high","temperature":0}`), false); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaPreservesIntegerEnumPrecision(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"seed":{"enum":[9007199254740993]}},"required":["seed"]}`)
	if _, err := Validate(raw, json.RawMessage(`{"seed":9007199254740993}`), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(raw, json.RawMessage(`{"seed":9007199254740992}`), false); err == nil {
		t.Fatal("schema integer enum rounded during credential expansion")
	}
}
