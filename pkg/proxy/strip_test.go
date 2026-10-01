package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripAuthHeaders(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com:6443/api", nil)
	req.Header.Set("Authorization", "Bearer stolen")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Proxy-Authorization", "Basic abc")
	req.Header.Set("Impersonate-User", "admin")
	req.Header.Set("Impersonate-Group", "system:masters")
	req.Header.Set("Impersonate-Uid", "123")
	req.Header.Set("Impersonate-Extra-scopes", "cluster-admin")
	req.Header.Set("X-Goog-Api-Key", "goog-key")
	req.Header.Set("Via", "evil")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	stripAuthHeaders(req)

	assert.Empty(t, req.Header.Get("Authorization"))
	assert.Empty(t, req.Header.Get("X-Api-Key"))
	assert.Empty(t, req.Header.Get("Proxy-Authorization"))
	assert.Empty(t, req.Header.Get("Impersonate-User"))
	assert.Empty(t, req.Header.Get("Impersonate-Group"))
	assert.Empty(t, req.Header.Get("Impersonate-Uid"))
	assert.Empty(t, req.Header.Get("Impersonate-Extra-scopes"))
	assert.Equal(t, "goog-key", req.Header.Get("X-Goog-Api-Key"))
	assert.Equal(t, "evil", req.Header.Get("Via"))
	assert.Equal(t, "1.2.3.4", req.Header.Get("X-Forwarded-For"))
}
