package config

import "testing"

func TestPublicReadOnlyModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, endpoint, model, want string
		invalid                           bool
	}{
		{name: "no provider", want: ModeLint},
		{name: "auto provider", endpoint: "https://provider.example/v1", model: "test", want: ModeReadOnly},
		{name: "explicit read-only", mode: ModeReadOnly, endpoint: "https://provider.example/v1", model: "test", want: ModeReadOnly},
		{name: "explicit lint", mode: ModeLint, endpoint: "https://provider.example/v1", model: "test", want: ModeLint},
		{name: "partial endpoint", endpoint: "https://provider.example/v1", invalid: true},
		{name: "partial model", model: "test", invalid: true},
		{name: "missing provider", mode: ModeReadOnly, invalid: true},
		{name: "public execution", mode: ModeReview, endpoint: "https://provider.example/v1", model: "test", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv(t.TempDir())
			env["PROBE_HUB_MODE"], env[EndpointEnvName], env[ModelEnvName] = tc.mode, tc.endpoint, tc.model
			c, err := Load(envOf(env))
			if (err != nil) != tc.invalid {
				t.Fatalf("Load error=%v, want invalid=%v", err, tc.invalid)
			}
			if !tc.invalid && (c.Mode != tc.want || c.ReviewAllowed(GitHub, "a/b", "digest")) {
				t.Fatalf("unexpected mode or execution permission: %+v", c)
			}
		})
	}
}
