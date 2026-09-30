package jira

// Conversion of Jira rich text into the Markdown subset that intent parsing
// reads: ATX headings, "-" and "1." list items (nested by two spaces), task
// checkboxes and fenced code. Formatting that carries no requirement (colour,
// emphasis, panels) is flattened to its text.

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// maxADFDepth bounds the recursion over an Atlassian Document.
const maxADFDepth = 64

// adfNode is one node of an Atlassian Document Format tree.
type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Attrs   json.RawMessage `json:"attrs"`
	Content []adfNode       `json:"content"`
}

func (n adfNode) attr(name string) string {
	var attrs map[string]any
	if json.Unmarshal(n.Attrs, &attrs) != nil {
		return ""
	}
	switch v := attrs[name].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// ADFToMarkdown converts an Atlassian Document (Jira Cloud, REST API v3).
func ADFToMarkdown(doc adfNode) string {
	var b strings.Builder
	adfBlocks(&b, doc.Content, "", 0)
	return collapseBlankLines(b.String())
}

// adfBlocks writes block nodes; indent prefixes every line (list nesting).
func adfBlocks(b *strings.Builder, nodes []adfNode, indent string, depth int) {
	if depth > maxADFDepth {
		return
	}
	for _, n := range nodes {
		switch n.Type {
		case "paragraph":
			writeLines(b, indent, adfInline(n.Content, depth+1))
			b.WriteString("\n")
		case "heading":
			level, _ := strconv.Atoi(n.attr("level"))
			if level < 1 || level > 6 {
				level = 2
			}
			b.WriteString(indent + strings.Repeat("#", level) + " " + oneLine(adfInline(n.Content, depth+1)) + "\n\n")
		case "bulletList", "orderedList", "taskList":
			adfList(b, n, indent, depth+1)
			b.WriteString("\n")
		case "codeBlock":
			b.WriteString(indent + "```" + oneLine(n.attr("language")) + "\n")
			writeLines(b, indent, adfInline(n.Content, depth+1))
			b.WriteString(indent + "```\n\n")
		case "blockquote", "panel", "expand", "nestedExpand", "layoutSection", "layoutColumn", "bodiedExtension":
			if title := oneLine(n.attr("title")); title != "" {
				writeLines(b, indent, title)
				b.WriteString("\n")
			}
			adfBlocks(b, n.Content, indent, depth+1)
		case "rule":
			b.WriteString("\n")
		case "table":
			adfTable(b, n, indent, depth+1)
			b.WriteString("\n")
		case "mediaSingle", "mediaGroup", "media":
			// Attachments carry no text.
		default:
			if len(n.Content) > 0 {
				adfBlocks(b, n.Content, indent, depth+1)
			} else if t := adfInline([]adfNode{n}, depth+1); strings.TrimSpace(t) != "" {
				writeLines(b, indent, t)
				b.WriteString("\n")
			}
		}
	}
}

func adfList(b *strings.Builder, list adfNode, indent string, depth int) {
	if depth > maxADFDepth {
		return
	}
	number := 1
	if start, err := strconv.Atoi(list.attr("order")); err == nil && start > 0 {
		number = start
	}
	for _, item := range list.Content {
		marker := "- "
		switch {
		case list.Type == "orderedList":
			marker = strconv.Itoa(number) + ". "
			number++
		case item.Type == "taskItem" && item.attr("state") == "DONE":
			marker = "- [x] "
		case item.Type == "taskItem":
			marker = "- [ ] "
		}
		// The first paragraph is the item text; later blocks nest under it.
		var text string
		rest := item.Content
		if item.Type == "taskItem" {
			text, rest = adfInline(item.Content, depth+1), nil
		} else if len(rest) > 0 && rest[0].Type == "paragraph" {
			text, rest = adfInline(rest[0].Content, depth+1), rest[1:]
		}
		b.WriteString(indent + marker + oneLine(text) + "\n")
		for _, child := range rest {
			switch child.Type {
			case "bulletList", "orderedList", "taskList":
				adfList(b, child, indent+"  ", depth+1)
			case "taskItem":
				adfList(b, adfNode{Type: "taskList", Content: []adfNode{child}}, indent+"  ", depth+1)
			default:
				var sub strings.Builder
				adfBlocks(&sub, []adfNode{child}, "", depth+1)
				if t := oneLine(sub.String()); t != "" {
					b.WriteString(indent + "  " + t + "\n")
				}
			}
		}
	}
}

func adfTable(b *strings.Builder, table adfNode, indent string, depth int) {
	for _, row := range table.Content {
		var cells []string
		for _, cell := range row.Content {
			var sub strings.Builder
			adfBlocks(&sub, cell.Content, "", depth+1)
			cells = append(cells, strings.ReplaceAll(oneLine(sub.String()), "|", "\\|"))
		}
		if len(cells) > 0 {
			b.WriteString(indent + "| " + strings.Join(cells, " | ") + " |\n")
		}
	}
}

// adfInline flattens inline nodes to text.
func adfInline(nodes []adfNode, depth int) string {
	if depth > maxADFDepth {
		return ""
	}
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text":
			b.WriteString(n.Text)
		case "hardBreak":
			b.WriteString("\n")
		case "mention", "emoji", "status", "placeholder":
			if t := n.attr("text"); t != "" {
				b.WriteString(t)
			} else {
				b.WriteString(n.attr("shortName"))
			}
		case "inlineCard", "blockCard", "embedCard":
			b.WriteString(n.attr("url"))
		case "date":
			b.WriteString(n.attr("timestamp"))
		default:
			if n.Text != "" {
				b.WriteString(n.Text)
			}
			b.WriteString(adfInline(n.Content, depth+1))
		}
	}
	return b.String()
}

func writeLines(b *strings.Builder, indent, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(indent + line + "\n")
	}
}

var (
	wikiHeading  = regexp.MustCompile(`^\s*h([1-6])\.\s+(.*)$`)
	wikiList     = regexp.MustCompile(`^\s*([*#-]+)\s+(.*)$`)
	wikiCode     = regexp.MustCompile(`^\s*\{(code|noformat)(:[^}]*)?\}\s*$`)
	wikiQuote    = regexp.MustCompile(`^\s*bq\.\s+(.*)$`)
	wikiMacro    = regexp.MustCompile(`\{(?:color|panel|quote|expand)(?::[^}]*)?\}`)
	wikiLink     = regexp.MustCompile(`\[([^|\]]+)\|([^\]]+)\]`)
	blankLines   = regexp.MustCompile(`\n{3,}`)
	wikiRuleLine = regexp.MustCompile(`^\s*-{4,}\s*$`)
)

// WikiToMarkdown converts Jira wiki markup (Jira Server and Data Center,
// REST API v2) line by line. Emphasis and colour markup stay as text.
func WikiToMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	inCode := false
	for _, line := range strings.Split(s, "\n") {
		if wikiCode.MatchString(line) {
			b.WriteString("```\n")
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString(line + "\n")
			continue
		}
		line = wikiMacro.ReplaceAllString(line, "")
		line = wikiLink.ReplaceAllString(line, "$1 ($2)")
		switch {
		case wikiRuleLine.MatchString(line):
			b.WriteString("\n")
		case wikiHeading.MatchString(line):
			m := wikiHeading.FindStringSubmatch(line)
			level, _ := strconv.Atoi(m[1])
			b.WriteString(strings.Repeat("#", level) + " " + strings.TrimSpace(m[2]) + "\n")
		case wikiQuote.MatchString(line):
			b.WriteString(wikiQuote.FindStringSubmatch(line)[1] + "\n")
		case wikiList.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "---"):
			m := wikiList.FindStringSubmatch(line)
			markers := m[1]
			indent := strings.Repeat("  ", len(markers)-1)
			marker := "- "
			if markers[len(markers)-1] == '#' {
				marker = "1. "
			}
			b.WriteString(indent + marker + m[2] + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	if inCode {
		b.WriteString("```\n")
	}
	return collapseBlankLines(b.String())
}

func collapseBlankLines(s string) string {
	return blankLines.ReplaceAllString(strings.TrimSpace(s), "\n\n") + "\n"
}
