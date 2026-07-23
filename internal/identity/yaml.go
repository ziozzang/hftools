package identity

import (
	"fmt"
	"sort"
	"strings"
)

// This is a deliberately tiny YAML reader/writer covering only the shape hftools
// writes: top-level scalar keys plus a single nested `trusted_keys` map. It keeps
// the project dependency-free. It is not a general YAML parser — flow style,
// lists, anchors, and multi-line scalars are not supported.

// parseYAML reads the config subset. Unknown top-level keys are ignored so the
// format can grow without breaking older binaries.
func parseYAML(data []byte) (*Config, error) {
	cfg := &Config{TrustedKeys: map[string]string{}}
	inTrusted := false
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		key, value, ok := splitKV(trimmed)
		if !ok {
			return nil, fmt.Errorf("line %d: expected 'key: value', got %q", i+1, trimmed)
		}
		if inTrusted && indented {
			if value == "" {
				return nil, fmt.Errorf("line %d: trusted key %q has no value", i+1, key)
			}
			cfg.TrustedKeys[key] = value
			continue
		}
		inTrusted = false
		switch key {
		case "signer":
			cfg.Signer = value
		case "key_path":
			cfg.KeyPath = value
		case "trusted_keys":
			if value != "" && value != "{}" {
				return nil, fmt.Errorf("line %d: trusted_keys must be a nested map", i+1)
			}
			inTrusted = true
		default:
			// Ignore unknown keys for forward compatibility.
		}
	}
	return cfg, nil
}

// splitKV splits "key: value" on the first colon, trimming and unquoting the
// value and stripping a trailing unquoted "# comment".
func splitKV(s string) (key, value string, ok bool) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:idx])
	value = strings.TrimSpace(s[idx+1:])
	if key == "" {
		return "", "", false
	}
	value = stripInlineComment(value)
	value = unquote(value)
	return key, value, true
}

func stripInlineComment(v string) string {
	if strings.HasPrefix(v, "\"") || strings.HasPrefix(v, "'") {
		return v // quoted values keep '#'
	}
	if idx := strings.Index(v, " #"); idx >= 0 {
		return strings.TrimSpace(v[:idx])
	}
	return v
}

func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// marshalYAML renders cfg back to the config subset with a small header.
func marshalYAML(c *Config) []byte {
	var b strings.Builder
	b.WriteString("# hftools signing configuration (~/.hftools/config.yaml)\n")
	b.WriteString("# Managed by `hftools key`; hand-edits to known fields are preserved.\n\n")
	b.WriteString("signer: " + yamlScalar(c.Signer) + "\n")
	if strings.TrimSpace(c.KeyPath) != "" {
		b.WriteString("key_path: " + yamlScalar(c.KeyPath) + "\n")
	}
	b.WriteString("\ntrusted_keys:\n")
	names := make([]string, 0, len(c.TrustedKeys))
	for name := range c.TrustedKeys {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		b.WriteString("  # (none yet — add with: hftools key trust <name> <pubkey>)\n")
	}
	for _, name := range names {
		b.WriteString("  " + yamlScalar(name) + ": " + yamlScalar(c.TrustedKeys[name]) + "\n")
	}
	return []byte(b.String())
}

// yamlScalar quotes a value only when needed to survive the reader (empty,
// leading/trailing space, or a character that would change parsing).
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if s != strings.TrimSpace(s) || strings.ContainsAny(s, ":#\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
