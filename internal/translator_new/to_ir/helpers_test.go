package to_ir

import (
	"encoding/json"
	"testing"
)

func TestCleanJsonSchemaEnhanced_Draft2020_12(t *testing.T) {
	schemaJSON := `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"type": "object",
		"properties": {
			"location": {
				"type": "string",
				"minLength": 1,
				"format": "city"
			},
			"pattern": {
				"type": "object",
				"properties": {
					"regex": { "type": "string", "pattern": "^[a-z]+$" }
				}
			},
			"unit": {
				"type": ["string", "null"],
				"default": "celsius"
			}
		},
		"required": ["location"]
	}`

	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	cleaned := CleanJsonSchemaEnhanced(schema)

	// 1. Verify type lowercased
	if cleaned["type"] != "object" {
		t.Errorf("Expected type object, got %v", cleaned["type"])
	}
	props := cleaned["properties"].(map[string]any)
	loc := props["location"].(map[string]any)
	if loc["type"] != "string" {
		t.Errorf("Expected location.type string, got %v", loc["type"])
	}

	// 2. Verify constraints migration
	if loc["minLength"] != nil {
		t.Errorf("minLength should be removed")
	}
	desc, _ := loc["description"].(string)
	if desc == "" { // Expecting constraint hint
		t.Error("Description should contain constraint hint")
	}

	// 3. Verify 'pattern' property preserved
	pat := props["pattern"].(map[string]any)
	if pat["type"] != "object" {
		t.Errorf("pattern.type should be object")
	}
	regex := pat["properties"].(map[string]any)["regex"].(map[string]any)
	if regex["pattern"] != nil {
		t.Error("inner pattern should be removed")
	}

	// 4. Verify union type fallback
	unit := props["unit"].(map[string]any)
	if unit["type"] != "string" {
		t.Errorf("Expected unit.type string, got %v", unit["type"])
	}

	// 5. Verify metadata removed
	if cleaned["$schema"] != nil {
		t.Error("$schema should be removed")
	}
}

func TestFlattenRefs(t *testing.T) {
	schemaJSON := `{
		"$defs": {
			"Address": {
				"type": "object",
				"properties": {
					"city": { "type": "string" }
				}
			}
		},
		"properties": {
			"home": { "$ref": "#/$defs/Address" }
		}
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	cleaned := CleanJsonSchemaEnhanced(schema)

	props := cleaned["properties"].(map[string]any)
	home := props["home"].(map[string]any)

	if home["type"] != "object" {
		t.Errorf("Expected home.type object (resolved from ref), got %v", home["type"])
	}
	city := home["properties"].(map[string]any)["city"].(map[string]any)
	if city["type"] != "string" {
		t.Errorf("Expected city.type string")
	}
	if home["$ref"] != nil {
		t.Error("$ref should be removed")
	}
}

func TestAnyOfTypeExtraction(t *testing.T) {
	schemaJSON := `{
		"type": "object",
		"properties": {
			"testo": {
				"anyOf": [
					{"type": "string"},
					{"type": "null"}
				],
				"default": null
			},
			"importo": {
				"anyOf": [
					{"type": "number"},
					{"type": "null"}
				]
			}
		}
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	cleaned := CleanJsonSchemaEnhanced(schema)
	props := cleaned["properties"].(map[string]any)

	testo := props["testo"].(map[string]any)
	if testo["type"] != "string" {
		t.Errorf("Expected testo.type string, got %v", testo["type"])
	}
	if testo["anyOf"] != nil {
		t.Error("anyOf should be removed")
	}

	importo := props["importo"].(map[string]any)
	if importo["type"] != "number" {
		t.Errorf("Expected importo.type number, got %v", importo["type"])
	}
}

func TestIssue815_AnyOfPropertiesPreserved(t *testing.T) {
	schemaJSON := `{
		"type": "object",
		"properties": {
			"config": {
				"anyOf": [
					{
						"type": "object",
						"properties": {
							"path": { "type": "string" },
							"recursive": { "type": "boolean" }
						},
						"required": ["path"]
					},
					{ "type": "null" }
				]
			}
		}
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	cleaned := CleanJsonSchemaEnhanced(schema)
	props := cleaned["properties"].(map[string]any)
	config := props["config"].(map[string]any)

	if config["type"] != "object" {
		t.Errorf("Expected config.type object, got %v", config["type"])
	}
	
	cProps := config["properties"].(map[string]any)
	if cProps["path"].(map[string]any)["type"] != "string" {
		t.Error("path property missing or wrong type")
	}
	if cProps["recursive"].(map[string]any)["type"] != "boolean" {
		t.Error("recursive property missing or wrong type")
	}

	req := config["required"].([]any)
	hasPath := false
	for _, r := range req {
		if r.(string) == "path" {
			hasPath = true
		}
	}
	if !hasPath {
		t.Error("required field 'path' missing")
	}
}

func TestDeeplyNestedMultiLevelDefs(t *testing.T) {
	schemaJSON := `{
		"type": "object",
		"$defs": {
			"RootDef": { "type": "integer" }
		},
		"properties": {
			"level1": {
				"type": "object",
				"$defs": {
					"Level1Def": { "type": "boolean" }
				},
				"properties": {
					"level2": {
						"type": "object",
						"$defs": {
							"Level2Def": { "type": "number" }
						},
						"properties": {
							"useRoot": { "$ref": "#/$defs/RootDef" },
							"useLevel1": { "$ref": "#/$defs/Level1Def" },
							"useLevel2": { "$ref": "#/$defs/Level2Def" }
						}
					}
				}
			}
		}
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	cleaned := CleanJsonSchemaEnhanced(schema)

	level1 := cleaned["properties"].(map[string]any)["level1"].(map[string]any)
	level2 := level1["properties"].(map[string]any)["level2"].(map[string]any)
	level2Props := level2["properties"].(map[string]any)

	if level2Props["useRoot"].(map[string]any)["type"] != "integer" {
		t.Error("RootDef resolution failed")
	}
	if level2Props["useLevel1"].(map[string]any)["type"] != "boolean" {
		t.Error("Level1Def resolution failed")
	}
	if level2Props["useLevel2"].(map[string]any)["type"] != "number" {
		t.Error("Level2Def resolution failed")
	}
}

func TestAllOfMerge(t *testing.T) {
	schemaJSON := `{
		"allOf": [
			{
				"properties": { "a": {"type": "string"} },
				"required": ["a"]
			},
			{
				"properties": { "b": {"type": "integer"} },
				"required": ["b"]
			}
		]
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	cleaned := CleanJsonSchemaEnhanced(schema)

	props := cleaned["properties"].(map[string]any)
	if props["a"].(map[string]any)["type"] != "string" {
		t.Error("Prop a missing")
	}
	if props["b"].(map[string]any)["type"] != "integer" {
		t.Error("Prop b missing")
	}

	req := cleaned["required"].([]any)
	if len(req) != 2 {
		t.Errorf("Expected 2 required fields, got %d", len(req))
	}
}

func TestFixToolCallArgs(t *testing.T) {
	argsJSON := `{
		"port": "8080",
		"enabled": "true",
		"timeout": "5.5",
		"metadata": {
			"retry": "3"
		},
		"tags": ["1", "2"]
	}`
	var args map[string]any
	json.Unmarshal([]byte(argsJSON), &args)

	schemaJSON := `{
		"properties": {
			"port": { "type": "integer" },
			"enabled": { "type": "boolean" },
			"timeout": { "type": "number" },
			"metadata": {
				"type": "object",
				"properties": {
					"retry": { "type": "integer" }
				}
			},
			"tags": {
				"type": "array",
				"items": { "type": "integer" }
			}
		}
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	FixToolCallArgs(args, schema)

	if port, ok := args["port"].(float64); !ok || port != 8080 {
		t.Errorf("port should be 8080 (float64), got %v (%T)", args["port"], args["port"])
	}
	if enabled, ok := args["enabled"].(bool); !ok || !enabled {
		t.Errorf("enabled should be true, got %v", args["enabled"])
	}
	if timeout, ok := args["timeout"].(float64); !ok || timeout != 5.5 {
		t.Errorf("timeout should be 5.5, got %v", args["timeout"])
	}
	
	meta, _ := args["metadata"].(map[string]any)
	if retry, ok := meta["retry"].(float64); !ok || retry != 3 {
		t.Errorf("metadata.retry should be 3, got %v", meta["retry"])
	}

	tags, _ := args["tags"].([]any)
	if len(tags) != 2 {
		t.Errorf("tags length should be 2")
	} else {
		if v, ok := tags[0].(float64); !ok || v != 1 {
			t.Errorf("tags[0] should be 1, got %v", tags[0])
		}
		if v, ok := tags[1].(float64); !ok || v != 2 {
			t.Errorf("tags[1] should be 2, got %v", tags[1])
		}
	}
}

func TestFixToolCallArgsProtection(t *testing.T) {
	argsJSON := `{
		"version": "01.0",
		"code": "007"
	}`
	var args map[string]any
	json.Unmarshal([]byte(argsJSON), &args)

	schemaJSON := `{
		"properties": {
			"version": { "type": "number" },
			"code": { "type": "integer" }
		}
	}`
	var schema map[string]any
	json.Unmarshal([]byte(schemaJSON), &schema)

	FixToolCallArgs(args, schema)

	// Should remain strings
	if v, ok := args["version"].(string); !ok || v != "01.0" {
		t.Errorf("version should remain '01.0', got %v", args["version"])
	}
	if c, ok := args["code"].(string); !ok || c != "007" {
		t.Errorf("code should remain '007', got %v", args["code"])
	}
}