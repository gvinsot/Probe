// Package msg marks the English messages the engine shows to people.
//
// M returns its argument unchanged: it only marks a message, or a fmt format,
// so that the i18n test finds it and requires a translation in every catalog
// of web/public/i18n/. The interface translates the text when it displays it,
// so a report saved by an earlier version, or in another language, is shown
// in the current one.
package msg

// M marks a message to translate and returns it unchanged.
func M(english string) string { return english }
