package routing

import (
	_ "embed"
	"strings"
)

// embeddedContextRegistry ships with the binary so every OAT install has the
// hand-curated `max_input_tokens` fallbacks even without access to the repo's
// model-routing/context-registry.yaml file at runtime. Update this YAML when
// providers change their context windows.
//
//go:embed context-registry.yaml
var embeddedContextRegistry string

// LoadEmbeddedContextRegistry parses the registry that shipped with the binary.
// Never returns an error under normal conditions — the embed directive
// guarantees the file is present at build time. If the file ever becomes
// malformed, the loader's tolerant parser will return a mostly-empty registry
// and a follow-on daemon load will surface the issue via Count() == 0.
func LoadEmbeddedContextRegistry() *ContextRegistry {
	r := &ContextRegistry{entries: map[string]*ContextRegistryEntry{}}
	parseRegistryContent(r, embeddedContextRegistry)
	return r
}

// parseRegistryContent shares the state-machine logic with LoadContextRegistry
// but works on an in-memory string. Kept separate from the file path so the
// embedded loader and the on-disk loader converge on the same parser without
// a tempfile dance.
func parseRegistryContent(r *ContextRegistry, content string) {
	var (
		inModelsBlock bool
		currentModel  string
		cur           *ContextRegistryEntry
	)
	flush := func() {
		if cur != nil && currentModel != "" {
			cur.ModelID = currentModel
			r.entries[currentModel] = cur
		}
		currentModel = ""
		cur = nil
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if !inModelsBlock {
			if strings.HasPrefix(trimmed, "models:") {
				inModelsBlock = true
			}
			continue
		}

		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
			flush()
			key := strings.TrimSuffix(trimmed, ":")
			key = strings.Trim(key, "\"")
			currentModel = key
			cur = &ContextRegistryEntry{}
			continue
		}

		if strings.HasPrefix(line, "    ") && cur != nil {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, "\"")
			switch key {
			case "max_input_tokens":
				if v, err := parseInt64Safe(val); err == nil {
					cur.MaxInputTokens = v
				}
			case "max_output_tokens":
				if v, err := parseInt64Safe(val); err == nil {
					cur.MaxOutputTokens = v
				}
			case "source":
				cur.Source = val
			case "last_verified":
				cur.LastVerified = val
			case "notes":
				cur.Notes = val
			}
			continue
		}

		if !strings.HasPrefix(line, " ") {
			inModelsBlock = false
			flush()
		}
	}
	flush()
}

// parseInt64Safe wraps strconv.ParseInt to return a sentinel error for empty strings.
func parseInt64Safe(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errEmpty
	}
	// Go 1.21+: strconv is the right tool here. Use ParseInt to match registry semantics.
	var out int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNotInt
		}
		out = out*10 + int64(c-'0')
	}
	return out, nil
}

var (
	errEmpty  = stringError("empty")
	errNotInt = stringError("not int")
)

type stringError string

func (e stringError) Error() string { return string(e) }
