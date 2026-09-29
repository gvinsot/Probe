package office

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
)

// node is a minimal XML tree: OOXML parts other than worksheets are small
// enough to be held in memory, and a tree keeps the extraction code readable.
// Names are local names; namespaces are irrelevant for the parts read here.
type node struct {
	name     string
	attrs    []xml.Attr
	children []*node
	chars    strings.Builder
}

type xmlElement = *node

func ioLimit(r io.Reader, n int64) io.Reader { return io.LimitReader(r, n) }

func parseTree(data []byte) *node {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	root := &node{name: "#document"}
	stack := []*node{root}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: t.Name.Local, attrs: t.Attr}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			stack[len(stack)-1].chars.Write(t)
		}
	}
	return root
}

// walkXML calls fn for every element of the document, in document order.
func walkXML(data []byte, fn func(xmlElement)) {
	parseTree(data).walk(fn)
}

func (n *node) walk(fn func(*node)) {
	for _, c := range n.children {
		fn(c)
		c.walk(fn)
	}
}

// attr returns the value of the attribute with the given local name.
func (n *node) attr(local string) string {
	for _, a := range n.attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// plainAttr returns the attribute without namespace prefix, and nsAttr the
// prefixed one: a slide id carries both "id" and "r:id".
func (n *node) plainAttr(local string) string {
	for _, a := range n.attrs {
		if a.Name.Local == local && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}

func (n *node) nsAttr(local string) string {
	for _, a := range n.attrs {
		if a.Name.Local == local && a.Name.Space != "" {
			return a.Value
		}
	}
	return ""
}

// hasAttr reports whether the attribute is present, whatever its value.
func (n *node) hasAttr(local string) bool {
	for _, a := range n.attrs {
		if a.Name.Local == local {
			return true
		}
	}
	return false
}

// text returns the character data of the element and its descendants.
func (n *node) text() string {
	var b strings.Builder
	var rec func(*node)
	rec = func(x *node) {
		b.WriteString(x.chars.String())
		for _, c := range x.children {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

// all returns the descendants with the given local name, in document order.
func (n *node) all(name string) []*node {
	var out []*node
	n.walk(func(c *node) {
		if c.name == name {
			out = append(out, c)
		}
	})
	return out
}

// first returns the first descendant with the given local name.
func (n *node) first(name string) *node {
	var found *node
	var rec func(*node) bool
	rec = func(x *node) bool {
		for _, c := range x.children {
			if c.name == name {
				found = c
				return true
			}
			if rec(c) {
				return true
			}
		}
		return false
	}
	rec(n)
	return found
}

// relationships maps the relationship ids of a .rels part to their targets,
// resolved against the directory of the source part.
func relationships(p *pkg, relsPath, baseDir string) map[string]string {
	out := map[string]string{}
	data, ok := p.read(relsPath)
	if !ok {
		return out
	}
	walkXML(data, func(el xmlElement) {
		if el.name != "Relationship" || el.attr("TargetMode") == "External" {
			return
		}
		out[el.attr("Id")] = resolvePart(baseDir, el.attr("Target"))
	})
	return out
}

// resolvePart turns a relationship target into a package part name.
func resolvePart(baseDir, target string) string {
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(target, "/")
	}
	parts := strings.Split(strings.TrimSuffix(baseDir, "/"), "/")
	if baseDir == "" {
		parts = nil
	}
	for _, seg := range strings.Split(target, "/") {
		switch seg {
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		case ".", "":
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
