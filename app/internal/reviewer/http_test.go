package reviewer

import "testing"

func TestHTTPExceptionKeepsURLRestrictions(t *testing.T) {
	for _, endpoint := range []string{"http://192.168.1.27:8000/v1", "http://llm.internal/v1", "http://[fd00::1]:8000/v1"} {
		if err := Validate(Options{Endpoint: endpoint, Model: "test"}); err == nil {
			t.Errorf("HTTP accepted without exception: %s", endpoint)
		}
		if err := Validate(Options{Endpoint: endpoint, Model: "test", AllowInsecureHTTP: true}); err != nil {
			t.Errorf("explicit HTTP refused: %v", err)
		}
	}
	for _, endpoint := range []string{"file:///tmp/provider", "ftp://llm.internal", "http://user:password@llm.internal/v1", "http://llm.internal/v1?key=secret", "http://llm.internal/v1#fragment"} {
		if err := Validate(Options{Endpoint: endpoint, Model: "test", AllowInsecureHTTP: true}); err == nil {
			t.Errorf("unsafe URL accepted with HTTP exception: %s", endpoint)
		}
	}
}
