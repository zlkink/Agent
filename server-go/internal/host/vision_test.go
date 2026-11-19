package host

import (
	"encoding/json"
	"testing"
)

func TestAnyHasVisionShapes(t *testing.T) {
	yes := []string{
		`true`,
		`"image"`,
		`["text","image"]`,
		`{"input":["text","image"],"output":["text"]}`,
		`{"vision":true}`,
		`{"input_modalities":["image"]}`,
	}
	for _, raw := range yes {
		if !anyHasVision(json.RawMessage(raw)) {
			t.Fatalf("%s should be vision", raw)
		}
	}
	no := []string{`false`, `["text"]`, `{"input":["text"],"output":["text"]}`, `{"vision":false}`}
	for _, raw := range no {
		if anyHasVision(json.RawMessage(raw)) {
			t.Fatalf("%s should not be vision", raw)
		}
	}
}
