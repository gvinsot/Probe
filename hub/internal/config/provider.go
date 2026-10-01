package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Model tuning of the deployment's provider, read the same way the CLI reads
// them: the hub forwards both to the binary it runs and imposes them on the
// requests its LLM gateway relays.
const (
	ProviderEnvName    = "PROBE_REVIEWER_PROVIDER"
	TemperatureEnvName = "PROBE_REVIEWER_TEMPERATURE"
)

// parseProvider reads PROBE_REVIEWER_PROVIDER: a JSON object sent as the
// request's "provider" field (OpenRouter's provider routing), or a
// comma-separated list of provider names, which becomes
// {"order": [...], "allow_fallbacks": false}. Empty sends nothing.
func parseProvider(v string) (json.RawMessage, error) {
	v = strings.TrimSpace(v)
	invalid := fmt.Errorf("%s must be a JSON object or a comma-separated list of provider names", ProviderEnvName)
	switch {
	case v == "":
		return nil, nil
	case len(v) > 4096:
		return nil, fmt.Errorf("%s exceeds 4096 bytes", ProviderEnvName)
	case strings.HasPrefix(v, "{"):
		var object map[string]json.RawMessage
		var compact bytes.Buffer
		if json.Unmarshal([]byte(v), &object) != nil || json.Compact(&compact, []byte(v)) != nil {
			return nil, invalid
		}
		return compact.Bytes(), nil
	}
	var names []string
	for _, name := range strings.Split(v, ",") {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 100 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./") != "" {
			return nil, invalid
		}
		names = append(names, name)
	}
	if len(names) > 16 {
		return nil, fmt.Errorf("%s lists more than 16 providers", ProviderEnvName)
	}
	return json.Marshal(map[string]any{"order": names, "allow_fallbacks": false})
}

// parseTemperature reads PROBE_REVIEWER_TEMPERATURE, between 0 and 2. Empty
// leaves the provider's default.
func parseTemperature(v string) (*float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(t) || t < 0 || t > 2 {
		return nil, fmt.Errorf("%s must be a number between 0 and 2", TemperatureEnvName)
	}
	return &t, nil
}
