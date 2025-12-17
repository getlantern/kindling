package kindling

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCloneRequest_NilBody(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	cloned := cloneRequest(req, "test", "test", []byte{})
	if cloned == req {
		t.Error("expected a new request, got the same pointer")
	}
	if cloned.Body != nil && cloned.Body != http.NoBody {
		t.Error("expected cloned body to be nil or http.NoBody")
	}
}

func TestCloneRequest_NoBody(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", http.NoBody)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	cloned := cloneRequest(req, "test", "test", []byte{})
	if cloned == req {
		t.Error("expected a new request, got the same pointer")
	}
	if cloned.Body != http.NoBody {
		t.Error("expected cloned body to be http.NoBody")
	}
}

func TestCloneRequest_WithBody(t *testing.T) {
	originalBody := "hello world"
	req, err := http.NewRequest("POST", "http://example.com", io.NopCloser(strings.NewReader(originalBody)))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	bodyBytes, _ := io.ReadAll(req.Body)

	cloned := cloneRequest(req, "test", "test", bodyBytes)

	// Both bodies should be readable and equal to originalBody
	origBodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read original body: %v", err)
	}
	if string(origBodyBytes) != originalBody {
		t.Errorf("expected original body %q, got %q", originalBody, string(origBodyBytes))
	}

	clonedBodyBytes, err := io.ReadAll(cloned.Body)
	if err != nil {
		t.Fatalf("failed to read cloned body: %v", err)
	}
	if string(clonedBodyBytes) != originalBody {
		t.Errorf("expected cloned body %q, got %q", originalBody, string(clonedBodyBytes))
	}
}
