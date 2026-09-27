package config

import "testing"

func TestHTTPExceptionRequiresDeploymentEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, value, endpoint string
		want, invalid         bool
	}{
		{name: "default"},
		{name: "disabled", value: "false"},
		{name: "operator endpoint", value: "true", endpoint: "http://llm.internal:8000/v1", want: true},
		{name: "policy endpoint cannot inherit permission", value: "true", invalid: true},
		{name: "blank endpoint", value: "true", endpoint: "  ", invalid: true},
		{name: "invalid boolean", value: "tru", endpoint: "http://llm.internal", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default("go")
			cfg.Reviewer.Endpoint = "http://repository-controlled.example/v1"
			env := map[string]string{AllowInsecureHTTPEnv: tc.value, EndpointEnv: tc.endpoint}
			runtime, err := cfg.ResolveReviewer(func(k string) string { return env[k] }, nil)
			if (err != nil) != tc.invalid || runtime.AllowInsecureHTTP != tc.want {
				t.Fatalf("allowed=%v error=%v", runtime.AllowInsecureHTTP, err)
			}
		})
	}
}
