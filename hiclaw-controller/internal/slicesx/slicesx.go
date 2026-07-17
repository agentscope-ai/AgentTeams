package slicesx

// Contains reports whether target is present in values.
func Contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// UniqueNonEmpty returns values with empty strings removed and duplicates collapsed
// while preserving first-seen order.
func UniqueNonEmpty(values []string) []string {
	return AppendUnique(nil, values...)
}

// AppendUnique appends non-empty values to base, skipping duplicates already
// present in base or earlier appended values, preserving first-seen order.
func AppendUnique(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	out := make([]string, 0, len(base)+len(values))
	for _, v := range base {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
