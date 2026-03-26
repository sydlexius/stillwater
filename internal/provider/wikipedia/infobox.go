package wikipedia

import (
	"strings"
	"unicode"
)

// infoboxData holds structured data extracted from a Wikipedia infobox.
type infoboxData struct {
	Origin      string
	YearsActive string
	Genres      []string
	Members     []string
	PastMembers []string
}

// parseInfobox extracts structured data from a wikitext infobox template.
// It recognizes "Infobox musical artist", "Infobox musician", and
// "Infobox person" templates. Returns nil if no recognized infobox is found.
func parseInfobox(wikitext string) *infoboxData {
	block := findInfoboxBlock(wikitext)
	if block == "" {
		return nil
	}

	fields := parseFields(block)
	if len(fields) == 0 {
		return nil
	}

	data := &infoboxData{}

	// Origin / birth_place
	if v := fieldValue(fields, "origin", "birth_place"); v != "" {
		data.Origin = cleanMarkup(v)
	}

	// Years active
	if v := fieldValue(fields, "years_active", "years active"); v != "" {
		data.YearsActive = cleanYearsActive(v)
	}

	// Genres
	if v := fieldValue(fields, "genre", "genres"); v != "" {
		data.Genres = parseListField(v)
	}

	// Current members
	if v := fieldValue(fields, "current_members", "members", "current members"); v != "" {
		data.Members = parseListField(v)
	}

	// Past members
	if v := fieldValue(fields, "past_members", "past members", "former_members", "former members"); v != "" {
		data.PastMembers = parseListField(v)
	}

	// Return nil if nothing was extracted.
	if data.Origin == "" && data.YearsActive == "" &&
		len(data.Genres) == 0 && len(data.Members) == 0 && len(data.PastMembers) == 0 {
		return nil
	}

	return data
}

// findInfoboxBlock locates the first recognized infobox template and returns
// its full content (everything between the opening {{ and closing }}).
// Uses brace-counting to handle nested templates.
func findInfoboxBlock(wikitext string) string {
	lower := strings.ToLower(wikitext)

	// Look for recognized infobox template names.
	prefixes := []string{
		"{{infobox musical artist",
		"{{infobox musician",
		"{{infobox person",
		"{{infobox singer",
		"{{infobox composer",
	}

	startIdx := -1
	for _, prefix := range prefixes {
		idx := strings.Index(lower, prefix)
		if idx >= 0 && (startIdx < 0 || idx < startIdx) {
			startIdx = idx
		}
	}
	if startIdx < 0 {
		return ""
	}

	// Walk forward from startIdx, counting braces to find the matching }}.
	depth := 0
	i := startIdx
	for i < len(wikitext) {
		if i+1 < len(wikitext) && wikitext[i] == '{' && wikitext[i+1] == '{' {
			depth++
			i += 2
			continue
		}
		if i+1 < len(wikitext) && wikitext[i] == '}' && wikitext[i+1] == '}' {
			depth--
			if depth == 0 {
				return wikitext[startIdx : i+2]
			}
			i += 2
			continue
		}
		i++
	}

	// Unmatched braces -- return what we have.
	return ""
}

// parseFields extracts the top-level pipe-delimited key=value pairs from
// an infobox block. Nested templates (depth > 1) are treated as part of
// the value, not as field separators.
func parseFields(block string) map[string]string {
	fields := make(map[string]string)

	// Strip the outer {{ ... }} and the template name line.
	inner := strings.TrimPrefix(block, "{{")
	inner = strings.TrimSuffix(inner, "}}")

	// Skip past the template name (first line or until first |).
	if idx := strings.Index(inner, "\n"); idx >= 0 {
		first := inner[:idx]
		if !strings.Contains(first, "|") || strings.Index(first, "|") > strings.Index(first, "infobox") {
			inner = inner[idx+1:]
		}
	}

	// Split on top-level pipes (depth 0 braces and brackets).
	// Walk the string tracking {{ }} and [[ ]] depth so that pipes inside
	// wikilinks (e.g. [[Richard Wright (musician)|Richard Wright]]) and
	// templates are not treated as field separators.
	var segments []string
	braceDepth := 0
	bracketDepth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		if i+1 < len(inner) && inner[i] == '{' && inner[i+1] == '{' {
			braceDepth++
			i++
			continue
		}
		if i+1 < len(inner) && inner[i] == '}' && inner[i+1] == '}' {
			braceDepth--
			if braceDepth < 0 {
				braceDepth = 0
			}
			i++
			continue
		}
		if i+1 < len(inner) && inner[i] == '[' && inner[i+1] == '[' {
			bracketDepth++
			i++
			continue
		}
		if i+1 < len(inner) && inner[i] == ']' && inner[i+1] == ']' {
			bracketDepth--
			if bracketDepth < 0 {
				bracketDepth = 0
			}
			i++
			continue
		}
		if inner[i] == '|' && braceDepth == 0 && bracketDepth == 0 {
			segments = append(segments, inner[start:i])
			start = i + 1
		}
	}
	segments = append(segments, inner[start:])

	for _, seg := range segments {
		key, value, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		// Skip keys that contain newlines (malformed segments).
		if strings.ContainsAny(key, "\n\r") {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" {
			fields[key] = value
		}
	}

	return fields
}

// fieldValue returns the first non-empty value for the given field name aliases.
func fieldValue(fields map[string]string, aliases ...string) string {
	for _, alias := range aliases {
		if v, ok := fields[alias]; ok && v != "" {
			return v
		}
	}
	return ""
}

// parseListField extracts a list of items from a wikitext field value.
// Handles: {{flatlist|...}}, {{hlist|...}}, {{plainlist|...}}, bullet lists,
// <br /> separators, and comma-separated values.
func parseListField(value string) []string {
	// Strip ref tags and their content first.
	value = stripRefs(value)

	// Unwrap list templates: flatlist, hlist, plainlist, unbulleted list.
	value = unwrapListTemplates(value)

	var items []string

	// After hlist unwrapping, items are pipe-separated (e.g. "[[Rock]]|[[Pop]]").
	// Split on pipes that are outside wikilinks and templates.
	if strings.Contains(value, "|") && !strings.Contains(value, "\n*") {
		pipeItems := splitOnTopLevelPipes(value)
		if len(pipeItems) > 1 {
			for _, part := range pipeItems {
				item := cleanMarkup(part)
				if item != "" {
					items = append(items, item)
				}
			}
			return items
		}
	}

	// Check for bullet-list items.
	if strings.Contains(value, "\n*") || strings.HasPrefix(strings.TrimSpace(value), "*") {
		lines := strings.Split(value, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "*") {
				item := strings.TrimLeft(line, "* ")
				item = cleanMarkup(item)
				if item != "" {
					items = append(items, item)
				}
			}
		}
		if len(items) > 0 {
			return items
		}
	}

	// Check for <br /> separated values.
	if strings.Contains(strings.ToLower(value), "<br") {
		parts := splitOnBR(value)
		for _, part := range parts {
			item := cleanMarkup(part)
			if item != "" {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			return items
		}
	}

	// Fall back to comma separation if the value looks like a comma list.
	if strings.Contains(value, ",") {
		parts := strings.Split(value, ",")
		for _, part := range parts {
			item := cleanMarkup(part)
			if item != "" {
				items = append(items, item)
			}
		}
		if len(items) > 1 {
			return items
		}
	}

	// Single value.
	item := cleanMarkup(value)
	if item != "" {
		return []string{item}
	}
	return nil
}

// unwrapListTemplates recursively removes wrapping list template syntax.
// Handles: {{flatlist|...}}, {{hlist|...}}, {{plainlist|...}}, {{unbulleted list|...}}.
func unwrapListTemplates(s string) string {
	lower := strings.ToLower(s)
	templates := []string{"flatlist", "hlist", "plainlist", "unbulleted list", "ubl"}

	for _, tmpl := range templates {
		prefix := "{{" + tmpl
		idx := strings.Index(lower, prefix)
		if idx < 0 {
			continue
		}

		// Find the pipe after the template name.
		afterName := idx + len(prefix)
		pipeIdx := strings.Index(s[afterName:], "|")
		if pipeIdx < 0 {
			continue
		}
		contentStart := afterName + pipeIdx + 1

		// Find matching closing braces.
		depth := 1
		i := afterName
		for i < len(s) {
			if i+1 < len(s) && s[i] == '{' && s[i+1] == '{' {
				depth++
				i += 2
				continue
			}
			if i+1 < len(s) && s[i] == '}' && s[i+1] == '}' {
				depth--
				if depth == 0 {
					// Extract the inner content.
					inner := s[contentStart:i]
					// Replace the whole template with its inner content.
					result := s[:idx] + inner + s[i+2:]
					// Recurse in case of nested list templates.
					return unwrapListTemplates(result)
				}
				i += 2
				continue
			}
			i++
		}
	}

	return s
}

// cleanMarkup removes wikitext markup from a string, producing plain text.
// Handles: [[Link|Display]] -> Display, [[Link]] -> Link,
// {{nowrap|text}} -> text, {{small|text}} -> text, HTML tags, ref tags.
func cleanMarkup(s string) string {
	s = stripRefs(s)
	s = stripHTMLTags(s)
	s = resolveWikilinks(s)
	s = stripSimpleTemplates(s)
	s = strings.TrimSpace(s)
	// Remove leading/trailing punctuation artifacts.
	s = strings.Trim(s, " ,;*")
	return s
}

// resolveWikilinks converts [[Link|Display]] to Display and [[Link]] to Link.
func resolveWikilinks(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '[' && s[i+1] == '[' {
			// Find the matching ]].
			end := strings.Index(s[i+2:], "]]")
			if end < 0 {
				b.WriteByte(s[i])
				i++
				continue
			}
			inner := s[i+2 : i+2+end]
			// Use display text if present (after |).
			if pipeIdx := strings.Index(inner, "|"); pipeIdx >= 0 {
				b.WriteString(inner[pipeIdx+1:])
			} else {
				b.WriteString(inner)
			}
			i = i + 2 + end + 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// stripSimpleTemplates removes common inline templates like {{nowrap|text}},
// {{small|text}}, {{lang|xx|text}}, keeping only the last pipe-delimited argument.
func stripSimpleTemplates(s string) string {
	simpleTemplates := []string{"nowrap", "small", "lang", "native name", "nihongo", "transl"}

	for _, tmpl := range simpleTemplates {
		searchFrom := 0
		for {
			lower := strings.ToLower(s)
			prefix := "{{" + tmpl
			idx := strings.Index(lower[searchFrom:], prefix)
			if idx < 0 {
				break
			}
			idx += searchFrom

			// Ensure the character after the template name is | or }
			afterName := idx + len(prefix)
			if afterName < len(s) && s[afterName] != '|' && s[afterName] != '}' {
				// Not an exact match (e.g. "smallcaps" vs "small"), skip.
				searchFrom = afterName
				continue
			}

			// Find matching }}.
			depth := 1
			i := afterName
			for i < len(s) {
				if i+1 < len(s) && s[i] == '{' && s[i+1] == '{' {
					depth++
					i += 2
					continue
				}
				if i+1 < len(s) && s[i] == '}' && s[i+1] == '}' {
					depth--
					if depth == 0 {
						inner := s[afterName:i]
						// Take the last pipe segment as the display text.
						parts := strings.Split(inner, "|")
						display := strings.TrimSpace(parts[len(parts)-1])
						s = s[:idx] + display + s[i+2:]
						break
					}
					i += 2
					continue
				}
				i++
			}
			if depth != 0 {
				break // Unmatched braces, stop processing this template.
			}
		}
	}

	// Strip any remaining unknown templates that are inline (no newlines).
	// Only remove simple single-level templates.
	for {
		idx := strings.Index(s, "{{")
		if idx < 0 {
			break
		}
		end := strings.Index(s[idx+2:], "}}")
		if end < 0 {
			break
		}
		inner := s[idx+2 : idx+2+end]
		// Only strip if it doesn't contain nested templates or newlines.
		if strings.Contains(inner, "{{") || strings.Contains(inner, "\n") {
			break
		}
		// If it contains a pipe, keep the last segment.
		if pipeIdx := strings.LastIndex(inner, "|"); pipeIdx >= 0 {
			display := strings.TrimSpace(inner[pipeIdx+1:])
			s = s[:idx] + display + s[idx+2+end+2:]
		} else {
			// Template with no pipe -- remove entirely.
			s = s[:idx] + s[idx+2+end+2:]
		}
	}

	return s
}

// splitOnTopLevelPipes splits s on pipe characters that are outside
// wikilinks ([[ ]]) and templates ({{ }}).
func splitOnTopLevelPipes(s string) []string {
	var parts []string
	braceDepth := 0
	bracketDepth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		if i+1 < len(s) && s[i] == '{' && s[i+1] == '{' {
			braceDepth++
			i++
			continue
		}
		if i+1 < len(s) && s[i] == '}' && s[i+1] == '}' {
			braceDepth--
			if braceDepth < 0 {
				braceDepth = 0
			}
			i++
			continue
		}
		if i+1 < len(s) && s[i] == '[' && s[i+1] == '[' {
			bracketDepth++
			i++
			continue
		}
		if i+1 < len(s) && s[i] == ']' && s[i+1] == ']' {
			bracketDepth--
			if bracketDepth < 0 {
				bracketDepth = 0
			}
			i++
			continue
		}
		if s[i] == '|' && braceDepth == 0 && bracketDepth == 0 {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// stripRefs removes <ref>...</ref> and <ref ... /> tags.
func stripRefs(s string) string {
	for {
		lower := strings.ToLower(s)
		idx := strings.Index(lower, "<ref")
		if idx < 0 {
			break
		}

		// Self-closing <ref ... />
		closeSlash := strings.Index(s[idx:], "/>")
		closeTag := strings.Index(lower[idx:], "</ref>")

		if closeSlash >= 0 && (closeTag < 0 || closeSlash < closeTag) {
			s = s[:idx] + s[idx+closeSlash+2:]
		} else if closeTag >= 0 {
			s = s[:idx] + s[idx+closeTag+6:]
		} else {
			// Malformed ref, just remove <ref.
			s = s[:idx] + s[idx+4:]
		}
	}
	return s
}

// splitOnBR splits a string on <br>, <br/>, <br />, and variants.
func splitOnBR(s string) []string {
	var parts []string
	lower := strings.ToLower(s)
	start := 0
	for {
		idx := strings.Index(lower[start:], "<br")
		if idx < 0 {
			parts = append(parts, s[start:])
			break
		}
		pos := start + idx
		parts = append(parts, s[start:pos])

		// Skip past the <br ... > or <br ... />.
		end := strings.Index(s[pos:], ">")
		if end >= 0 {
			start = pos + end + 1
		} else {
			start = pos + 3
		}
		lower = strings.ToLower(s) // refresh after advancing
	}
	return parts
}

// cleanYearsActive normalizes a years_active value.
// Strips markup and normalizes common patterns like "1985-present", "{{start date|1985}}".
func cleanYearsActive(s string) string {
	s = stripRefs(s)
	s = stripHTMLTags(s)

	// Handle {{start date|YYYY}} template.
	lower := strings.ToLower(s)
	if strings.Contains(lower, "{{start date") {
		idx := strings.Index(lower, "{{start date")
		end := strings.Index(s[idx:], "}}")
		if end >= 0 {
			inner := s[idx+2 : idx+end]
			parts := strings.Split(inner, "|")
			if len(parts) >= 2 {
				year := strings.TrimSpace(parts[1])
				// Replace the template with just the year.
				s = s[:idx] + year + s[idx+end+2:]
			}
		}
	}

	s = resolveWikilinks(s)
	s = stripSimpleTemplates(s)

	// Normalize dashes: en-dash and em-dash to hyphen.
	s = strings.ReplaceAll(s, "\u2013", "-") // en-dash
	s = strings.ReplaceAll(s, "\u2014", "-") // em-dash

	s = strings.TrimSpace(s)

	// Collapse multiple spaces.
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}

	return s
}

// stripHTMLTags removes HTML tags from s, returning plain text.
// Wikipedia's "displaytitle" field can contain markup like <i>Name</i>.
func stripHTMLTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			b.WriteRune(r)
		}
	}
	return collapseWhitespace(b.String())
}

// collapseWhitespace replaces runs of whitespace with a single space.
func collapseWhitespace(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
			}
			prevSpace = true
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
