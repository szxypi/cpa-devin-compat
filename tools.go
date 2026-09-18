package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// flattenNamespaceTools expands Responses namespace tools into ordinary top-level
// declarations. It handles both the standard top-level tools field and Codex
// Desktop's Responses Lite input[].type=additional_tools representation.
//
// CPA's Responses conversion otherwise keeps a namespace as a grouped tool. The
// Devin executor reads name/description/parameters from each tool entry, so the
// group can become an unnamed MCP tool and Devin rejects the request. Flattening
// preserves function/custom child declarations so CPA can perform its normal
// downstream conversion. Namespace children colliding with an earlier tool name
// are skipped; top-level tools take precedence over additional_tools.
func flattenNamespaceTools(body []byte) ([]byte, int, int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, 0, 0
	}

	out := body
	seen := make(map[string]bool)
	namespaces := 0
	flattened := 0
	changed := false

	flattenArray := func(tools gjson.Result) ([]byte, int, int, bool) {
		if !tools.IsArray() {
			return nil, 0, 0, false
		}

		items := make([]string, 0, len(tools.Array()))
		arrayNamespaces := 0
		arrayFlattened := 0
		arrayChanged := false

		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) != "namespace" {
				items = append(items, tool.Raw)
				if name := responsesRequestToolName(tool); name != "" {
					seen[name] = true
				}
				continue
			}

			arrayChanged = true
			arrayNamespaces++
			children := tool.Get("tools")
			if !children.IsArray() {
				children = tool.Get("children")
			}
			for _, child := range children.Array() {
				childType := strings.TrimSpace(child.Get("type").String())
				if childType != "" && childType != "function" && childType != "custom" {
					continue
				}
				name := responsesRequestToolName(child)
				if name == "" || seen[name] {
					continue
				}

				raw := child.Raw
				if childType == "" {
					if normalized, err := sjson.Set(raw, "type", "function"); err == nil {
						raw = normalized
					}
				}
				items = append(items, raw)
				seen[name] = true
				arrayFlattened++
			}
		}

		if !arrayChanged {
			return nil, 0, 0, false
		}
		return []byte("[" + strings.Join(items, ",") + "]"), arrayNamespaces, arrayFlattened, true
	}

	root := gjson.ParseBytes(out)
	if tools := root.Get("tools"); tools.IsArray() {
		if rewritten, n, f, ok := flattenArray(tools); ok {
			var err error
			out, err = sjson.SetRawBytes(out, "tools", rewritten)
			if err == nil {
				namespaces += n
				flattened += f
				changed = true
			}
		}
	}

	// Reparse after a possible top-level rewrite, while retaining the shared seen
	// set so top-level declarations win over duplicates in additional_tools.
	input := gjson.GetBytes(out, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			rewritten, n, f, ok := flattenArray(item.Get("tools"))
			if !ok {
				continue
			}
			var err error
			out, err = sjson.SetRawBytes(out, fmt.Sprintf("input.%d.tools", index), rewritten)
			if err != nil {
				continue
			}
			namespaces += n
			flattened += f
			changed = true
		}
	}

	if !changed {
		return body, 0, 0
	}
	return out, namespaces, flattened
}

func responsesRequestToolName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

// sanitizeResponsesToolSchemas inlines local JSON Schema references in
// Responses function tools. Devin accepts ordinary oneOf/anyOf schemas, but
// rejects some real Codex schemas that retain a large $defs/$ref graph (for
// example automation_update). Inlining keeps the same reachable constraints
// while removing the provider-incompatible reference graph.
func sanitizeResponsesToolSchemas(body []byte) ([]byte, int, int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, 0, 0
	}

	out := body
	schemas := 0
	refs := 0
	changed := false

	if tools := gjson.GetBytes(out, "tools"); tools.IsArray() {
		if rewritten, n, r, ok := sanitizeResponsesToolArray(tools.Raw); ok {
			var err error
			out, err = sjson.SetRawBytes(out, "tools", rewritten)
			if err == nil {
				schemas += n
				refs += r
				changed = true
			}
		}
	}

	// Reparse after replacing top-level tools because sjson may have allocated
	// a new body. Responses Lite stores additional tools in input items.
	input := gjson.GetBytes(out, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			tools := item.Get("tools")
			if !tools.IsArray() {
				continue
			}
			rewritten, n, r, ok := sanitizeResponsesToolArray(tools.Raw)
			if !ok {
				continue
			}
			var err error
			out, err = sjson.SetRawBytes(out, fmt.Sprintf("input.%d.tools", index), rewritten)
			if err != nil {
				continue
			}
			schemas += n
			refs += r
			changed = true
		}
	}

	if !changed {
		return body, 0, 0
	}
	return out, schemas, refs
}

func sanitizeResponsesToolArray(raw string) ([]byte, int, int, bool) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	var tools []any
	if err := decoder.Decode(&tools); err != nil {
		return nil, 0, 0, false
	}

	schemas := 0
	refs := 0
	changed := false
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if strings.TrimSpace(toolType) != "function" {
			continue
		}

		if schema, ok := tool["parameters"].(map[string]any); ok {
			rewritten, r, schemaChanged := inlineLocalSchemaRefs(schema)
			if schemaChanged {
				tool["parameters"] = rewritten
				schemas++
				refs += r
				changed = true
			}
			continue
		}

		// Also tolerate Chat-style function wrappers if one appears in a
		// Responses namespace child.
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		schema, ok := function["parameters"].(map[string]any)
		if !ok {
			continue
		}
		rewritten, r, schemaChanged := inlineLocalSchemaRefs(schema)
		if schemaChanged {
			function["parameters"] = rewritten
			schemas++
			refs += r
			changed = true
		}
	}

	if !changed {
		return nil, 0, 0, false
	}
	out, err := json.Marshal(tools)
	if err != nil {
		return nil, 0, 0, false
	}
	return out, schemas, refs, true
}

type localSchemaInliner struct {
	root    map[string]any
	active  map[string]bool
	refs    int
	changed bool
}

func inlineLocalSchemaRefs(schema map[string]any) (map[string]any, int, bool) {
	inliner := localSchemaInliner{
		root:   schema,
		active: make(map[string]bool),
	}
	rewritten, _ := inliner.rewrite(schema).(map[string]any)
	return rewritten, inliner.refs, inliner.changed
}

func (inliner *localSchemaInliner) rewrite(value any) any {
	switch current := value.(type) {
	case []any:
		out := make([]any, len(current))
		for i, item := range current {
			out[i] = inliner.rewrite(item)
		}
		return out
	case map[string]any:
		if ref, ok := current["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			inliner.refs++
			inliner.changed = true

			// A recursive edge cannot be finitely expanded. Replacing only that
			// edge with {} preserves a valid permissive schema and prevents an
			// unbounded request expansion.
			if inliner.active[ref] {
				return inliner.rewriteSchemaSiblings(current)
			}
			if target, ok := resolveLocalSchemaRef(inliner.root, ref); ok {
				inliner.active[ref] = true
				resolved := inliner.rewrite(target)
				delete(inliner.active, ref)
				return mergeSchemaRefSiblings(resolved, inliner.rewriteSchemaSiblings(current))
			}

			// A dangling local ref cannot be useful to the upstream provider.
			// Keep any sibling constraints and otherwise degrade to {}.
			return inliner.rewriteSchemaSiblings(current)
		}

		out := make(map[string]any, len(current))
		for key, item := range current {
			if key == "$defs" || key == "definitions" {
				inliner.changed = true
				continue
			}
			out[key] = inliner.rewrite(item)
		}
		return out
	default:
		return value
	}
}

func (inliner *localSchemaInliner) rewriteSchemaSiblings(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for key, item := range schema {
		if key == "$ref" || key == "$defs" || key == "definitions" {
			continue
		}
		out[key] = inliner.rewrite(item)
	}
	return out
}

func mergeSchemaRefSiblings(resolved any, siblings map[string]any) any {
	base, ok := resolved.(map[string]any)
	if !ok {
		if len(siblings) == 0 {
			return resolved
		}
		return siblings
	}
	out := make(map[string]any, len(base)+len(siblings))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range siblings {
		out[key] = value
	}
	return out
}

func resolveLocalSchemaRef(root map[string]any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var current any = root
	for _, encoded := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
