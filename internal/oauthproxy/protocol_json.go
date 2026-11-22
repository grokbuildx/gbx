package oauthproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

// Preserve JSON numbers when a protocol envelope must be edited. In particular,
// schema enum/const values must not pass through float64 on model routing.
func decodeProtocolJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values in protocol payload")
	}
	return nil
}

// structuredOutputSchema renders a requested json_schema output in the shape
// both OpenAI dialects share (they only nest it differently). The schema is
// retained and strict mode downgraded for optional properties rather than
// silently making them required. Returns nil for json_object or absent formats.
func structuredOutputSchema(format *conversationFormat) map[string]any {
	if format == nil {
		return nil
	}
	if format.Type != "" && format.Type != "json_schema" {
		return nil
	}
	schemaRaw := stripSchemaMetaKey(format.Schema)
	strict := format.Strict == nil || *format.Strict
	strict = strict && schemaSupportsStrictOutput(gjson.ParseBytes(schemaRaw))
	result := map[string]any{"name": format.Name, "strict": strict, "schema": schemaRaw}
	if format.Description != "" {
		result["description"] = format.Description
	}
	return result
}

// structuredOutputPromptInstruction builds a system-prompt structured-output
// constraint for backends without native response_format (Anthropic, Kiro,
// Cursor). Supports both json_object and json_schema formats.
func structuredOutputPromptInstruction(format *conversationFormat) string {
	if format == nil {
		return ""
	}
	if format.Type == "json_object" {
		return "Respond only with a valid JSON object, with no other text and no markdown code fences."
	}
	schema := structuredOutputSchema(format)
	if schema == nil {
		return ""
	}
	encoded, _ := json.Marshal(schema["schema"])
	var b strings.Builder
	b.WriteString("Respond only with a raw JSON value (no prose, no markdown code fences) that matches this JSON Schema")
	if format.Name != "" {
		b.WriteString(" (name: ")
		b.WriteString(format.Name)
		b.WriteByte(')')
	}
	if format.Description != "" {
		b.WriteString(" — ")
		b.WriteString(format.Description)
	}
	b.WriteString(":\n")
	b.Write(encoded)
	return b.String()
}

func stripSchemaMetaKey(raw json.RawMessage) json.RawMessage {
	if !gjson.GetBytes(raw, "$schema").Exists() {
		return raw
	}
	var obj map[string]any
	if decodeProtocolJSON(raw, &obj) != nil || obj == nil {
		return raw
	}
	delete(obj, "$schema")
	cleaned, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return json.RawMessage(cleaned)
}

func schemaSupportsStrictOutput(schema gjson.Result) bool {
	return schemaSupportsStrictOutputAt(schema, true)
}

func schemaTypeContains(typeField gjson.Result, want string) bool {
	if typeField.Type == gjson.String {
		return typeField.String() == want
	}
	if typeField.IsArray() {
		for _, elem := range typeField.Array() {
			if elem.String() == want {
				return true
			}
		}
	}
	return false
}

func schemaSupportsStrictOutputAt(schema gjson.Result, root bool) bool {
	if !schema.IsObject() {
		return !root
	}
	// Keywords unsupported by OpenAI strict structured outputs.
	for _, forbidden := range []string{
		"oneOf", "not", "if", "then", "else",
		"patternProperties", "dependentSchemas", "propertyNames",
		"contains", "prefixItems",
	} {
		if schema.Get(forbidden).Exists() {
			return false
		}
	}
	properties := schema.Get("properties")
	isObject := properties.IsObject() || schemaTypeContains(schema.Get("type"), "object")
	if root && !isObject && !schema.Get("$ref").Exists() {
		return false
	}
	if isObject {
		if schema.Get("additionalProperties").Type != gjson.False {
			return false
		}
		propMap := properties.Map()
		required := make(map[string]bool)
		for _, name := range schema.Get("required").Array() {
			key := name.String()
			if len(propMap) > 0 {
				if _, exists := propMap[key]; !exists {
					return false
				}
			} else {
				return false
			}
			required[key] = true
		}
		for name := range propMap {
			if !required[name] {
				return false
			}
		}
	}
	// Only traverse schema-bearing keywords, not enum/const example objects.
	for _, keyword := range []string{"properties", "$defs", "definitions"} {
		for _, child := range schema.Get(keyword).Map() {
			if !schemaSupportsStrictOutputAt(child, false) {
				return false
			}
		}
	}
	for _, keyword := range []string{"items", "additionalProperties"} {
		if child := schema.Get(keyword); child.Exists() {
			if !schemaSupportsStrictOutputAt(child, false) {
				return false
			}
		}
	}
	for _, keyword := range []string{"allOf", "anyOf"} {
		for _, child := range schema.Get(keyword).Array() {
			if !schemaSupportsStrictOutputAt(child, false) {
				return false
			}
		}
	}
	return true
}
