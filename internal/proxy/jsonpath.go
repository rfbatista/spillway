package proxy

import "strings"

// jsonPathVisitor is applied to every value a JSON path matches. It returns the
// replacement for that slot, or drop=true to remove the key from its parent
// object. An array element is never removed, only replaced.
type jsonPathVisitor func(v any) (replacement any, drop bool)

// splitJSONPath splits a dot-separated path into segments. A blank path yields
// no segments, so it is a no-op rather than a match on the empty key. A JSON key
// containing a dot cannot be addressed.
func splitJSONPath(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return strings.Split(path, ".")
}

// applyJSONPath walks segments from node and applies visit to every value the
// path matches, mutating node in place.
//
// Arrays are traversed implicitly: when a segment lands on a JSON array, the
// remaining segments are applied to every element, so "messages.content.text"
// reaches messages[].content[].text without any wildcard syntax. A segment that
// is missing, or that lands on a scalar, is a no-op, so a stale or mistyped path
// never panics and never touches an unrelated field.
func applyJSONPath(node any, segments []string, visit jsonPathVisitor) {
	if len(segments) == 0 || visit == nil {
		return
	}

	switch n := node.(type) {
	case []any:
		for _, elem := range n {
			applyJSONPath(elem, segments, visit)
		}
	case map[string]any:
		v, ok := n[segments[0]]
		if !ok {
			return
		}
		if len(segments) > 1 {
			applyJSONPath(v, segments[1:], visit)
			return
		}
		if replacement, drop := visit(v); drop {
			delete(n, segments[0])
		} else {
			n[segments[0]] = replacement
		}
	}
}
