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
	cloned := cloneRequest(req)
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
	cloned := cloneRequest(req)
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
	cloned := cloneRequest(req)

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

func TestCloneRequest_BodyReadError(t *testing.T) {
	// Simulate a body that returns an error on Read
	errBody := &errorReader{}
	req, err := http.NewRequest("POST", "http://example.com", errBody)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	cloned := cloneRequest(req)
	// Should return the original request if error occurs
	if cloned != req {
		t.Error("expected cloneRequest to return original request on error")
	}
}

// errorReader simulates an io.Reader that always returns an error
type errorReader struct{}

func (e *errorReader) Read(p []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (e *errorReader) Close() error {
	return nil
}
