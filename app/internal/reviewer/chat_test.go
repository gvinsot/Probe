package reviewer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// A non-streaming completion sends its headers only once generated: the wait
// for them is the whole reviewer timeout, not a fixed shorter one.
func TestResponseHeaderWaitIsTheReviewerTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, 90 * time.Second, 20 * time.Minute} {
		o, endpoint, err := normalize(Options{Endpoint: "https://provider.example/v1", Model: "m", Timeout: timeout})
		if err != nil {
			t.Fatal(err)
		}
		c := newChat(o, endpoint)
		if c.transport.ResponseHeaderTimeout != o.Timeout || o.Timeout < 90*time.Second {
			t.Errorf("timeout %s: response header wait %s, reviewer timeout %s", timeout, c.transport.ResponseHeaderTimeout, o.Timeout)
		}
		c.close()
	}
}

func TestSlowEndpointWithinTheTimeoutSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		complete(w)
	}))
	defer server.Close()
	r := &model.Report{}
	if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", Timeout: 5 * time.Second}, r, &fakeHarness{}); err != nil {
		t.Fatal(err)
	}
}

// A timeout names itself, and never echoes the endpoint.
func TestTimeoutErrorSaysSo(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test", Timeout: 200 * time.Millisecond}, &model.Report{}, &fakeHarness{})
	if err == nil || !strings.Contains(err.Error(), "gave no response within") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestErrorWording(t *testing.T) {
	if err := requestError(errors.New("dial tcp 10.0.0.1:8000: connection refused"), time.Second, time.Minute); !strings.Contains(err.Error(), "transport error") || strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("err = %v", err)
	}
	if err := requestError(context.DeadlineExceeded, 61*time.Second, 5*time.Minute); err.Error() != "reviewer endpoint gave no response within 1m1s (reviewer timeout 5m0s); the model may be slow or overloaded" {
		t.Fatalf("err = %v", err)
	}
}
