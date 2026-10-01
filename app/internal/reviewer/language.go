package reviewer

import (
	"errors"
	"regexp"
)

// languagePattern accepts a language name ("French", "Português (Brasil)"),
// never instructions: it is written into the system prompt.
var languagePattern = regexp.MustCompile(`^\p{L}[\p{L} ()-]{0,39}$`)

// ValidateLanguage checks a report language; empty means English.
func ValidateLanguage(language string) error {
	if language != "" && !languagePattern.MatchString(language) {
		return errors.New("report language must be a language name of at most 40 letters, spaces, hyphens or parentheses")
	}
	return nil
}

// languageInstruction tells the model which language its prose is read in.
// What Probe parses or checks stays as it is: keys and enum values, and the
// quotes it looks for in the diff.
func languageInstruction(language string) string {
	return "\n\nWrite every natural-language text you produce for the reader (titles, explanations, rationales, summaries, plans) in " + language +
		". Keep JSON keys, enum values, tool names, identifiers, file paths, code and quoted code exactly as they are, untranslated."
}

// localize returns messages with the language instruction appended to the
// leading system prompt; the caller's slice is not modified.
func localize(messages []message, language string) []message {
	if language == "" || len(messages) == 0 || messages[0].Role != "system" {
		return messages
	}
	out := append([]message(nil), messages...)
	out[0].Content += languageInstruction(language)
	return out
}
