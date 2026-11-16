package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParsedImageFieldsPreserveWorkerDecodeBoundary(t *testing.T) {
	for _, raw := range []string{
		`{"images":[{"mime_type":"image/png","data":"eA=="}]}`,
		`{"images":[{"mime_type":"image/png","data":"eA==","extra":"unsupported"}]}`,
		`{"images":[{"mime_type":"image/png","data":"invalid base64"}]}`,
		`{"images":[{"mime_type":"image/png","data":""}]}`,
		`{"images":[{"mime_type":"text/plain","data":"eA=="}]}`,
	} {
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeImageFields(fields)
		_, legacy, legacyErr := DecodeInput(json.RawMessage(raw))
		if (err == nil) != (legacyErr == nil) || !reflect.DeepEqual(decoded, legacy) {
			t.Fatalf("image boundary changed: parsed=%v/%v worker=%v/%v", decoded, err, legacy, legacyErr)
		}
	}
}
