package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Bounds of PROBE_REVIEWER_PROVIDER.
const (
	maxProviderBytes = 4096
	maxProviderNames = 16
)

// ParseProvider reads the provider routing of PROBE_REVIEWER_PROVIDER. A JSON
// object is OpenRouter's provider preferences, sent as they are. Otherwise the
// value is a comma-separated list of provider names, which restricts the
// request to those providers, tried in that order:
// {"order": [...], "allow_fallbacks": false}.
func ParseProvider(v string) (json.RawMessage, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if len(v) > maxProviderBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxProviderBytes)
	}
	if strings.HasPrefix(v, "{") {
		var object map[string]json.RawMessage
		d := json.NewDecoder(strings.NewReader(v))
		if err := d.Decode(&object); err != nil || d.More() {
			return nil, errors.New("must be a JSON object or a comma-separated list of provider names")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(v)); err != nil {
			return nil, errors.New("must be a JSON object or a comma-separated list of provider names")
		}
		return compact.Bytes(), nil
	}
	var names []string
	for _, name := range strings.Split(v, ",") {
		name = strings.TrimSpace(name)
		if !providerName(name) {
			return nil, fmt.Errorf("invalid provider name %q", name)
		}
		names = append(names, name)
	}
	if len(names) > maxProviderNames {
		return nil, fmt.Errorf("lists more than %d providers", maxProviderNames)
	}
	return json.Marshal(struct {
		Order          []string `json:"order"`
		AllowFallbacks bool     `json:"allow_fallbacks"`
	}{names, false})
}

// providerName accepts provider slugs such as "anthropic", "google-vertex" or
// "deepinfra/turbo".
func providerName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./", r)) {
			return false
		}
	}
	return true
}

// ParseTemperature reads PROBE_REVIEWER_TEMPERATURE: the sampling temperature,
// between 0 and 2 as the Chat Completions API accepts it.
func ParseTemperature(v string) (float64, error) {
	t, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(t) || t < 0 || t > 2 {
		return 0, errors.New("must be a number between 0 and 2")
	}
	return t, nil
}
