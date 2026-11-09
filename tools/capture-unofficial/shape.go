package main

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// shape replaces every value in a decoded JSON document with its type, so the
// result shows the fields an endpoint returns and none of the account's data:
// strings become "string", numbers "number", and so on. Elements of an array
// are merged into one shape, and a field whose type varies lists each type, as
// in "number|null".
func shape(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			key = redactKey(key)
			if existing, ok := out[key]; ok {
				out[key] = merge(existing, shape(item))
				continue
			}
			out[key] = shape(item)
		}
		return out
	case []any:
		if len(v) == 0 {
			return []any{}
		}
		merged := shape(v[0])
		for _, item := range v[1:] {
			merged = merge(merged, shape(item))
		}
		return []any{merged}
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "unknown"
}

// dataKey matches keys that are data rather than field names: dates, numeric
// ids, and long hex or UUID ids.
var dataKey = regexp.MustCompile(`^(?:[0-9].*|[0-9A-Fa-f-]{16,})$`)

// redactKey hides a key that carries the account's data, such as a map keyed
// by date or by activity id.
func redactKey(key string) string {
	if dataKey.MatchString(key) {
		return "(key)"
	}
	return key
}

// merge combines two shapes of the same field.
func merge(a, b any) any {
	ma, aIsMap := a.(map[string]any)
	mb, bIsMap := b.(map[string]any)
	if aIsMap && bIsMap {
		out := maps.Clone(ma)
		for key, item := range mb {
			if existing, ok := out[key]; ok {
				out[key] = merge(existing, item)
				continue
			}
			out[key] = item
		}
		return out
	}
	la, aIsList := a.([]any)
	lb, bIsList := b.([]any)
	if aIsList && bIsList {
		switch {
		case len(la) == 0:
			return lb
		case len(lb) == 0:
			return la
		}
		return []any{merge(la[0], lb[0])}
	}
	// A field that is null in one record and an object in another keeps the
	// object's fields and notes that it can be null. The null may already be
	// merged with other types, as in "null|string".
	if aIsMap && canBeNull(b) {
		return withNull(ma)
	}
	if bIsMap && canBeNull(a) {
		return withNull(mb)
	}
	// An array has nowhere to note it, so it just keeps its element's shape.
	if aIsList && canBeNull(b) || bIsList && canBeNull(a) {
		if aIsList {
			return la
		}
		return lb
	}
	sa, aIsString := a.(string)
	sb, bIsString := b.(string)
	if aIsString && bIsString {
		return joinTypes(sa, sb)
	}
	// An object or array against a scalar: keep the richer shape.
	if aIsMap || aIsList {
		return a
	}
	return b
}

func canBeNull(shape any) bool {
	types, ok := shape.(string)
	return ok && slices.Contains(strings.Split(types, "|"), "null")
}

func withNull(m map[string]any) map[string]any {
	out := maps.Clone(m)
	out["(nullable)"] = true
	return out
}

func joinTypes(a, b string) string {
	types := append(strings.Split(a, "|"), strings.Split(b, "|")...)
	sort.Strings(types)
	return strings.Join(slices.Compact(types), "|")
}
