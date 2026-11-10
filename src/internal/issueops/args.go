package issueops

import "strconv"

// Args are the arguments of an operation, keyed by the names of the MCP
// tool's arguments. The getters have the semantics of the MCP library's
// request getters (mark3labs/mcp-go CallToolRequest), so an operation reads
// an argument exactly as the tool did.
type Args map[string]any

// GetArguments returns the arguments as a map.
func (a Args) GetArguments() map[string]any { return a }

// GetString returns a string argument, or defaultValue when it is absent or
// not a string.
func (a Args) GetString(key, defaultValue string) string {
	if v, ok := a[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return defaultValue
}

// RequireString returns a string argument, or an error when it is absent or
// not a string.
func (a Args) RequireString(key string) (string, error) {
	if v, ok := a[key]; ok {
		if s, ok := v.(string); ok {
			return s, nil
		}
		return "", &argError{key: key, what: "is not a string"}
	}
	return "", &argError{key: key, what: "not found"}
}

// GetInt returns an int argument (an int, a float64 or a numeric string), or
// defaultValue.
func (a Args) GetInt(key string, defaultValue int) int {
	if v, ok := a[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return defaultValue
}

// GetFloat returns a float64 argument (a float64, an int or a numeric
// string), or defaultValue.
func (a Args) GetFloat(key string, defaultValue float64) float64 {
	if v, ok := a[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return f
			}
		}
	}
	return defaultValue
}

// GetBool returns a bool argument (a bool, a parseable string, a non-zero
// number), or defaultValue.
func (a Args) GetBool(key string, defaultValue bool) bool {
	if v, ok := a[key]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			if p, err := strconv.ParseBool(b); err == nil {
				return p
			}
		case int:
			return b != 0
		case float64:
			return b != 0
		}
	}
	return defaultValue
}

// GetStringSlice returns a string slice argument — its string items — or
// defaultValue.
func (a Args) GetStringSlice(key string, defaultValue []string) []string {
	if v, ok := a[key]; ok {
		switch s := v.(type) {
		case []string:
			return s
		case []any:
			out := make([]string, 0, len(s))
			for _, item := range s {
				if str, ok := item.(string); ok {
					out = append(out, str)
				}
			}
			return out
		}
	}
	return defaultValue
}

// GetIntSlice returns an int slice argument — its numeric items — or
// defaultValue.
func (a Args) GetIntSlice(key string, defaultValue []int) []int {
	if v, ok := a[key]; ok {
		switch s := v.(type) {
		case []int:
			return s
		case []any:
			out := make([]int, 0, len(s))
			for _, item := range s {
				switch n := item.(type) {
				case int:
					out = append(out, n)
				case float64:
					out = append(out, int(n))
				case string:
					if i, err := strconv.Atoi(n); err == nil {
						out = append(out, i)
					}
				}
			}
			return out
		}
	}
	return defaultValue
}

type argError struct{ key, what string }

func (e *argError) Error() string { return "argument " + strconv.Quote(e.key) + " " + e.what }
