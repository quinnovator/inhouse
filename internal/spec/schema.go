package spec

import (
	_ "embed"
	"encoding/json"
)

//go:embed stack.schema.json
var schema []byte

// Schema returns the stack spec's JSON Schema.
func Schema() []byte { return schema }

// DeployInputSchema is the MCP input schema shared by deploy and plan_deploy:
// a spec given either as a YAML string or as a JSON object.
func DeployInputSchema() json.RawMessage {
	var stack map[string]any
	if err := json.Unmarshal(schema, &stack); err != nil {
		panic(err)
	}
	delete(stack, "$schema")
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"spec": map[string]any{"anyOf": []any{
				map[string]any{"type": "string", "description": "Stack spec as YAML"},
				stack,
			}},
			"idempotency_key": map[string]any{"type": "string", "maxLength": 128, "description": "Retrying with the same key returns the same operation"},
		},
		"required":             []string{"spec"},
		"additionalProperties": false,
	}
	out, _ := json.Marshal(input)
	return out
}
