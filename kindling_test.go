package kindling

import "testing"

func TestKindling(t *testing.T) {
	t.Parallel()

	t.Run("NewKindling", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			kindling := NewKindling()
			if kindling == nil {
				t.Errorf("NewKindling() = nil; want non-nil Kindling")
			}
		})
	})

	t.Run("kindling.NewHTTPClient", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			kindling := NewKindling()
			httpClient := kindling.NewHTTPClient()
			if httpClient == nil {
				t.Errorf("kindling.NewHTTPClient() = nil; want non-nil *http.Client")
			}
		})
	})
}
