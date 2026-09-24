package mcp

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The input schemas are hand-written (schema.go) and the handlers decode into
// their own structs with unknown fields refused (decodeArgs). Nothing but this
// test keeps the two in step, and the cost of drift is asymmetric in the worst
// way: a property the schema advertises and the struct lacks is a field every
// agent is told it may send and every handler refuses; a struct field the
// schema does not advertise is a capability no agent can discover, because
// additionalProperties is false.

// toolArgs maps each tool to the struct its handler decodes into.
var toolArgs = map[string]any{
	// Renderers (renderers.go).
	"list_renderers":   listRenderersArgs{},
	"play_here":        playHereArgs{},
	"control_playback": controlPlaybackArgs{},
	"playback_status":  playbackStatusArgs{},

	// Library (tools_library.go).
	"search_content":   searchContentArgs{},
	"discover_content": discoverContentArgs{},
	"get_external_ids": getExternalIDsArgs{},
	"browse_library":   browseLibraryArgs{},
	"list_artists":     groupingArgs{},
	"list_authors":     groupingArgs{},
	"continue_rail":    continueRailArgs{},

	// Acquisition (tools_acquisition.go).
	"want_content":             wantContentArgs{},
	"monitor_content":          monitorContentArgs{},
	"search_releases":          searchReleasesArgs{},
	"acquire_release":          acquireReleaseArgs{},
	"get_missing_content":      getMissingContentArgs{},
	"get_upgrade_candidates":   getUpgradeCandidatesArgs{},
	"get_content_satisfaction": getContentSatisfactionArgs{},
	"get_acquisition_status":   getAcquisitionStatusArgs{},
	"list_jobs":                listJobsArgs{},
	"explain_release":          explainReleaseArgs{},

	// Followed sources (tools_followed.go).
	"follow_source":         followSourceArgs{},
	"unfollow":              unfollowArgs{},
	"poll_source":           pollSourceArgs{},
	"set_source_profile":    setSourceProfileArgs{},
	"followed_source_items": followedSourceItemsArgs{},

	// Fabric (tools_fabric.go).
	"get_provider_status": struct{}{},
	"get_peer_status":     struct{}{},
	"get_replica_status":  getReplicaStatusArgs{},
	"sync_peer":           syncPeerArgs{},
	"verify_blob":         verifyBlobArgs{},
}

// toolsNotDecodingArgs are tools whose handler never reads its arguments, so
// there is no struct to hold the schema to. Each entry is a known gap, not a
// pattern to follow: a handler that ignores its arguments silently accepts a
// misspelled or unsupported one, which decodeArgs exists to refuse.
var toolsNotDecodingArgs = map[string]string{
	"list_followed": "listFollowed ignores its arguments; the limit its schema advertises has no effect",
}

func TestToolSchemasMatchTheirArgs(t *testing.T) {
	t.Parallel()
	s := &Server{tools: newRegistry()}
	s.registerTools()

	registered := map[string]bool{}
	for _, tool := range s.tools.all() {
		registered[tool.Name] = true
		if _, known := toolsNotDecodingArgs[tool.Name]; known {
			continue
		}
		args, ok := toolArgs[tool.Name]
		if !ok {
			t.Errorf("%s has no entry in toolArgs: name the struct its handler decodes into", tool.Name)
			continue
		}
		if tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: the schema must say additionalProperties false — decodeArgs refuses unknown fields", tool.Name)
		}
		for _, problem := range schemaDrift(tool.InputSchema, reflect.TypeOf(args), tool.Name) {
			t.Error(problem)
		}
	}
	for name := range toolArgs {
		if !registered[name] {
			t.Errorf("toolArgs names %s, which is not a registered tool", name)
		}
	}
	for name := range toolsNotDecodingArgs {
		if !registered[name] {
			t.Errorf("toolsNotDecodingArgs names %s, which is not a registered tool", name)
		}
	}
}

// schemaDrift compares an object schema's properties with a struct's JSON
// fields, in both directions and by JSON type, recursing into nested objects
// and arrays of objects that both sides describe.
func schemaDrift(schema map[string]any, typ reflect.Type, path string) []string {
	var problems []string
	props, _ := schema["properties"].(map[string]any)
	fields := jsonFields(typ)

	for _, name := range sortedKeys(props) {
		field, ok := fields[name]
		if !ok {
			problems = append(problems, path+"."+name+": the schema advertises it but the args struct has no such "+
				"field, so the handler refuses it as unknown")
			continue
		}
		prop, _ := props[name].(map[string]any)
		want, _ := prop["type"].(string)
		if got := jsonType(field); got != want {
			problems = append(problems, path+"."+name+": the schema says "+want+" but the struct decodes "+got)
			continue
		}
		switch want {
		case "object":
			if _, described := prop["properties"]; described && indirect(field).Kind() == reflect.Struct {
				problems = append(problems, schemaDrift(prop, indirect(field), path+"."+name)...)
			}
		case "array":
			items, _ := prop["items"].(map[string]any)
			elem := indirect(field).Elem()
			if items == nil {
				break
			}
			itemType, _ := items["type"].(string)
			if got := jsonType(elem); got != itemType {
				problems = append(problems, path+"."+name+"[]: the schema says "+itemType+" but the struct decodes "+got)
				break
			}
			if _, described := items["properties"]; described && indirect(elem).Kind() == reflect.Struct {
				problems = append(problems, schemaDrift(items, indirect(elem), path+"."+name+"[]")...)
			}
		}
	}
	for _, name := range sortedKeys(fields) {
		if _, ok := props[name]; !ok {
			problems = append(problems, path+"."+name+": the args struct decodes it but the schema does not "+
				"advertise it, so no agent can discover it")
		}
	}
	return problems
}

// jsonFields is a struct's fields by JSON name. Every exported field must carry
// a json tag: without one the wire name is the Go name, matched
// case-insensitively, which is not a contract anyone meant to publish.
func jsonFields(typ reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = "<untagged " + f.Name + ">"
		}
		out[name] = f.Type
	}
	return out
}

func indirect(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// jsonType is the JSON Schema type encoding/json decodes into t.
func jsonType(t reflect.Type) string {
	switch indirect(t).Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return indirect(t).Kind().String()
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
