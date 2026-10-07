package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allTools returns the registry with every optional container tool enabled.
func allTools() []mcpTool {
	s := newSession("/l", "/d")
	s.statusCmd = "status"
	s.enableCmd = "enable"
	s.disableCmd = "disable"
	return newMCP(s).tools
}

func readDoc(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestToolsDocumentedInREADME(t *testing.T) {
	readme := readDoc(t, "README.md")
	for _, tool := range allTools() {
		pattern := regexp.MustCompile("(?m)^### .*\\x60xdbg_" + regexp.QuoteMeta(tool.Name) + "\\(")
		if !pattern.Match(readme) {
			t.Errorf("README.md has no heading for tool %q", tool.Name)
		}
	}
}

func TestToolsDocumentedInSkill(t *testing.T) {
	skill := string(readDoc(t, "skills/xdbg/SKILL.md"))
	start := strings.Index(skill, "## Conversation Patterns")
	if start < 0 {
		t.Fatal("SKILL.md has no Conversation Patterns section")
	}
	conversation := skill[start:]
	if next := strings.Index(conversation[1:], "\n## "); next >= 0 {
		conversation = conversation[:next+1]
	}
	conversationTools := map[string]bool{
		"breakpoint_remove": true,
		"breakpoint_clear":  true,
		"step_out":          true,
		"pause":             true,
		"property_set":      true,
	}
	for _, tool := range allTools() {
		search := skill
		if conversationTools[tool.Name] {
			search = conversation
		}
		pattern := regexp.MustCompile("(?m)(^|[^A-Za-z0-9_])(xdbg_)?" + regexp.QuoteMeta(tool.Name) + "([^A-Za-z0-9_]|$)")
		if !pattern.MatchString(search) {
			t.Errorf("SKILL.md does not document tool %q in its required section", tool.Name)
		}
	}
}

func TestREADMEHasNoUnknownTools(t *testing.T) {
	readme := readDoc(t, "README.md")
	known := make(map[string]bool)
	for _, tool := range allTools() {
		known[tool.Name] = true
	}
	for _, match := range regexp.MustCompile(`xdbg_([a-z_]+)\(`).FindAllSubmatch(readme, -1) {
		name := string(match[1])
		if !known[name] {
			t.Errorf("README.md documents xdbg_%s, but the server has no such tool", name)
		}
	}
}

func TestREADMEToolParamsMatchSchema(t *testing.T) {
	readme := readDoc(t, "README.md")
	heading := regexp.MustCompile("\x60xdbg_([a-z_]+)\\(([^)]*)\\)\x60")
	docParams := make(map[string][]string)
	generic := regexp.MustCompile("<[^>]*>")
	for _, line := range strings.Split(string(readme), "\n") {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		for _, match := range heading.FindAllStringSubmatch(line, -1) {
			name := match[1]
			params := strings.TrimSpace(generic.ReplaceAllString(match[2], ""))
			var names []string
			if params != "" {
				for _, param := range strings.Split(params, ",") {
					fields := strings.Fields(param)
					if len(fields) < 2 {
						t.Errorf("%s: cannot parse README parameter %q", name, param)
						continue
					}
					names = append(names, fields[len(fields)-1])
				}
			}
			if _, exists := docParams[name]; exists {
				t.Errorf("README.md has duplicate tool heading for %q", name)
			}
			docParams[name] = names
		}
	}

	for _, tool := range allTools() {
		properties, ok := tool.InputSchema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: input schema has no properties object", tool.Name)
			continue
		}
		documented, exists := docParams[tool.Name]
		if !exists {
			t.Errorf("%s: README.md has no tool heading", tool.Name)
			continue
		}
		seen := make(map[string]bool, len(documented))
		for _, name := range documented {
			if seen[name] {
				t.Errorf("%s: README.md repeats parameter %q", tool.Name, name)
			}
			seen[name] = true
			if _, ok := properties[name]; !ok {
				t.Errorf("%s: README.md parameter %q is not in the input schema", tool.Name, name)
			}
		}
		expected := make([]string, 0, len(properties))
		for name := range properties {
			expected = append(expected, name)
		}
		sort.Strings(expected)
		sort.Strings(documented)
		if len(documented) != len(expected) {
			t.Errorf("%s: README params %v, schema params %v", tool.Name, documented, expected)
			continue
		}
		for i := range expected {
			if documented[i] != expected[i] {
				t.Errorf("%s: README params %v, schema params %v", tool.Name, documented, expected)
				break
			}
		}
	}
}
