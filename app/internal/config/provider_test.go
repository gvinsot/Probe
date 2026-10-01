package config

import (
	"io/fs"
	"strings"
	"testing"
)

func TestParseProvider(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "",
		"anthropic":                             `{"order":["anthropic"],"allow_fallbacks":false}`,
		" deepinfra/turbo , google-vertex ":     `{"order":["deepinfra/turbo","google-vertex"],"allow_fallbacks":false}`,
		`{ "only": ["groq"], "sort": "price" }`: `{"only":["groq"],"sort":"price"}`,
	} {
		got, err := ParseProvider(in)
		if err != nil || string(got) != want {
			t.Errorf("ParseProvider(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"a,,b", "has space", `{"order":`, `{"a":1} {"b":2}`, `{"a":1}}`, "x" + strings.Repeat(",x", 16), strings.Repeat("a", 5000)} {
		if got, err := ParseProvider(in); err == nil {
			t.Errorf("ParseProvider(%q) accepted as %s", in, got)
		}
	}
}

func TestParseTemperature(t *testing.T) {
	for in, want := range map[string]float64{"0": 0, " 0.2 ": 0.2, "2": 2} {
		if got, err := ParseTemperature(in); err != nil || got != want {
			t.Errorf("ParseTemperature(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-0.1", "2.01", "NaN", "Inf", "warm"} {
		if _, err := ParseTemperature(in); err == nil {
			t.Errorf("ParseTemperature(%q) accepted", in)
		}
	}
}

// Routing and temperature tune the deployment's model: both are reported as
// sources, an invalid value fails the run, and the Probe Hub gateway, which
// imposes its own model, receives no local routing.
func TestResolveReviewerModelTuning(t *testing.T) {
	const creds = "/home/a/probe-credentials.json"
	read := func(name string) ([]byte, error) {
		if name != creds {
			return nil, fs.ErrNotExist
		}
		return []byte(`{"hub":"https://hub.example","endpoint":"https://hub.example/llm/v1","model":"hub-model","token":"probe_mcp.a.b","login":"octocat"}`), nil
	}
	c := Default("go")
	env := map[string]string{ProviderEnv: "anthropic", TemperatureEnv: "0.3", "PROBE_CREDENTIALS_FILE": "off", "PROBE_API_KEY": "provider-key"}
	got, err := c.ResolveReviewer(func(k string) string { return env[k] }, read)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Provider) != `{"order":["anthropic"],"allow_fallbacks":false}` || got.Temperature == nil || *got.Temperature != 0.3 {
		t.Fatalf("resolved provider %s temperature %v", got.Provider, got.Temperature)
	}
	joined := strings.Join(got.Sources, "; ")
	if !strings.Contains(joined, "provider routing from "+ProviderEnv) || !strings.Contains(joined, "temperature from "+TemperatureEnv) {
		t.Fatalf("sources are %q", joined)
	}
	for name, value := range map[string]string{ProviderEnv: "bad name", TemperatureEnv: "3"} {
		if _, err := c.ResolveReviewer(func(k string) string {
			if k == name {
				return value
			}
			return ""
		}, nil); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: error %v", name, value, err)
		}
	}
	delete(env, "PROBE_API_KEY")
	env["PROBE_CREDENTIALS_FILE"] = creds
	got, err = c.ResolveReviewer(func(k string) string { return env[k] }, read)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != "https://hub.example/llm/v1" || got.Provider != nil || got.Temperature == nil {
		t.Fatalf("through the gateway: %+v", got)
	}
}
