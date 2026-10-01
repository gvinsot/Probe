package reviewer

// Deterministic repair of a model's JSON answer, tried before asking the
// model to correct it. It only fixes how the text is written, never what it
// says: what was cut off is dropped, not invented.

import (
	"strings"
)

// repairJSON returns the first JSON object of text, rewritten so that the
// mistakes models commonly make in long structured answers parse:
//
//   - prose before or after the object;
//   - raw newlines, tabs and other control characters inside strings;
//   - backslashes that start no valid escape, as in quoted code ("\_", "\d");
//   - commas before a closing brace or bracket;
//   - an answer cut off by the token limit, which is cut back to its last
//     complete member and closed.
//
// It returns false when text holds no object.
func repairJSON(text string) (string, bool) {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "", false
	}
	var out strings.Builder
	var stack []byte // the closing brackets still owed
	// A cut point is where the object may be truncated and closed: before a
	// comma, or just after an opening bracket, with the brackets then owed.
	type cut struct {
		at     int
		stack  string
		opener bool
	}
	var cuts []cut
	inString := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case c == '\\':
				if i+1 >= len(text) {
					out.WriteString(`\\`)
					continue
				}
				next := text[i+1]
				switch {
				case strings.IndexByte(`"\/bfnrt`, next) >= 0:
					out.WriteByte('\\')
					out.WriteByte(next)
					i++
				case next == 'u' && i+5 < len(text) && isHex(text[i+2:i+6]):
					out.WriteString(text[i : i+6])
					i += 5
				default:
					out.WriteString(`\\`)
				}
			case c == '"':
				inString = false
				out.WriteByte(c)
			case c == '\n':
				out.WriteString(`\n`)
			case c == '\r':
				out.WriteString(`\r`)
			case c == '\t':
				out.WriteString(`\t`)
			case c < 0x20:
				out.WriteString(" ")
			default:
				out.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out.WriteByte(c)
		case '{', '[':
			closing := byte('}')
			if c == '[' {
				closing = ']'
			}
			stack = append(stack, closing)
			out.WriteByte(c)
			cuts = append(cuts, cut{out.Len(), string(stack), true})
		case '}', ']':
			trimTrailingComma(&out)
			if len(stack) == 0 || stack[len(stack)-1] != c {
				// A stray closer: the object is malformed beyond repair here.
				return out.String(), false
			}
			stack = stack[:len(stack)-1]
			out.WriteByte(c)
			if len(stack) == 0 {
				return out.String(), true
			}
		case ',':
			cuts = append(cuts, cut{out.Len(), string(stack), false})
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	// Cut off: keep what precedes the last complete member and close it.
	if len(cuts) == 0 {
		return "", false
	}
	// An element opened just before the cut would be left empty: the cut
	// moves before it.
	written := out.String()
	n := len(cuts) - 1
	for n > 0 && cuts[n].opener {
		before := strings.TrimRight(written[:cuts[n].at-1], " \t\r\n")
		if before == "" || (before[len(before)-1] != ',' && before[len(before)-1] != '[') {
			break
		}
		n--
	}
	last := cuts[n]
	result := written[:last.at]
	var closed strings.Builder
	closed.WriteString(strings.TrimRight(strings.TrimSpace(result), ","))
	for i := len(last.stack) - 1; i >= 0; i-- {
		closed.WriteByte(last.stack[i])
	}
	return closed.String(), true
}

// trimTrailingComma removes a comma, and the space after it, at the end of
// what was written.
func trimTrailingComma(b *strings.Builder) {
	s := b.String()
	trimmed := strings.TrimRight(s, " \t\r\n")
	if strings.HasSuffix(trimmed, ",") {
		b.Reset()
		b.WriteString(trimmed[:len(trimmed)-1])
	}
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
