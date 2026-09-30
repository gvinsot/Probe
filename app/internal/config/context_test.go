package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func contextPolicy(t *testing.T, context string) (Config, error) {
	t.Helper()
	c := Default("go")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return Decode([]byte(strings.TrimSuffix(string(data), "}") + `,"context":` + context + `}`))
}

func TestContextDecodesNamesAndObjects(t *testing.T) {
	c, err := contextPolicy(t, `{
		"repos": ["company/shared-types", {"name": "company/payment-sdk", "ref": "v2", "url": "https://github.com/company/payment-sdk.git", "paths": ["src/**"], "role": "payment SDK"}],
		"clusters": ["payments"],
		"cluster_definitions": {"payments": {"description": "Payment service and its clients", "repos": ["company/payment-service", "company/shared-types"]}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Context == nil || c.Context.Repos[0].Name != "company/shared-types" || c.Context.Repos[1].Ref != "v2" || c.Context.Repos[1].Role != "payment SDK" {
		t.Fatalf("context %+v", c.Context)
	}
	resolved, err := c.Context.ResolveContext(nil, "company/payment-service")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0].Name != "company/shared-types" || strings.Join(resolved[0].Clusters, ",") != "payments" || len(resolved[1].Clusters) != 0 {
		t.Fatalf("the repository itself is left out and a repeated one merged: %+v", resolved)
	}
}

func TestContextRejectsInvalidDeclarations(t *testing.T) {
	for name, context := range map[string]string{
		"unknown field":   `{"repos": [{"name": "a/b", "branch": "x"}]}`,
		"bad name":        `{"repos": ["../etc"]}`,
		"credential url":  `{"repos": [{"name": "a/b", "url": "https://user:token@github.com/a/b.git"}]}`,
		"file url":        `{"repos": [{"name": "a/b", "url": "file:///tmp/b"}]}`,
		"option ref":      `{"repos": [{"name": "a/b", "ref": "--upload-pack=x"}]}`,
		"absolute path":   `{"repos": [{"name": "a/b", "paths": ["/etc/**"]}]}`,
		"bad cluster":     `{"clusters": ["Payments!"]}`,
		"empty cluster":   `{"cluster_definitions": {"payments": {"repos": []}}}`,
		"multi-line role": `{"repos": [{"name": "a/b", "role": "a\nb"}]}`,
	} {
		if _, err := contextPolicy(t, context); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := contextPolicy(t, `{"repos": [{"name": "a/b", "url": "git@github.com:a/b.git"}]}`); err != nil {
		t.Errorf("scp-like ssh url refused: %v", err)
	}
}

func TestResolveContextWithSharedClusters(t *testing.T) {
	shared, err := DecodeClusters([]byte(`{"clusters": {"platform": {"description": "Shared libraries", "repos": ["company/logging", {"name": "company/shared-types", "paths": ["api/**"]}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c := &Context{Repos: []ContextRepo{{Name: "company/shared-types", Role: "types"}}, Clusters: []string{"platform"}}
	resolved, err := c.ResolveContext(shared, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0].Role != "types" || len(resolved[0].Paths) != 0 || strings.Join(resolved[0].Clusters, ",") != "platform" {
		t.Fatalf("the first listing's settings win and clusters accumulate: %+v", resolved)
	}
	if _, err := (&Context{Clusters: []string{"missing"}}).ResolveContext(shared, ""); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("an unknown cluster must be refused, naming the known ones: %v", err)
	}
	both := &Context{Clusters: []string{"platform"}, ClusterDefinitions: map[string]Cluster{"platform": {Repos: []ContextRepo{{Name: "x/y"}}}}}
	if _, err := both.ResolveContext(shared, ""); err == nil {
		t.Fatal("a cluster defined twice must be refused")
	}
	if _, err := DecodeClusters([]byte(`{"clusters": {"p": {"repos": ["a/b"]}}, "extra": 1}`)); err == nil {
		t.Fatal("unknown key accepted in the clusters file")
	}
	var none *Context
	if got, err := none.ResolveContext(nil, ""); got != nil || err != nil {
		t.Fatal("no context")
	}
}
