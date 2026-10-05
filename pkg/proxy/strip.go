package proxy

import (
	"net/http"
	"path"
	"strings"
)

// authHeaders are removed before injection. X-Goog-Api-Key is not one of them.
var authHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"Proxy-Authorization",
	"Impersonate-User",
	"Impersonate-Group",
	"Impersonate-Uid",
}

func stripAuthHeaders(req *http.Request) {
	for _, header := range authHeaders {
		req.Header.Del(header)
	}
	for key := range req.Header {
		if strings.HasPrefix(strings.ToLower(key), "impersonate-extra-") {
			req.Header.Del(key)
		}
	}
}

func kubePathDenied(urlPath string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	if clean == "/" || clean == "." {
		return false
	}
	subresource, verb := parseKubePath(clean)
	if verb == "proxy" {
		return true
	}
	switch subresource {
	case "exec", "attach", "portforward", "proxy":
		return true
	default:
		return false
	}
}

// parseKubePath returns the subresource and a leading special verb (proxy or
// watch). The segment layout matches the Kubernetes apiserver: a resource or
// namespace name is not a subresource, and extra path after proxy stays on
// that subresource.
func parseKubePath(cleanPath string) (subresource, verb string) {
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	if len(parts) < 3 || (parts[0] != "api" && parts[0] != "apis") {
		return "", ""
	}
	prefix := parts[0]
	parts = parts[1:]
	if prefix == "apis" {
		if len(parts) < 3 {
			return "", ""
		}
		parts = parts[1:]
	}
	parts = parts[1:]
	if len(parts) == 0 {
		return "", ""
	}
	if parts[0] == "proxy" || parts[0] == "watch" {
		if len(parts) < 2 {
			return "", ""
		}
		verb = parts[0]
		parts = parts[1:]
	}
	if len(parts) > 2 && parts[0] == "namespaces" && parts[2] != "status" && parts[2] != "finalize" {
		parts = parts[2:]
	}
	if verb == "proxy" || len(parts) < 3 {
		return "", verb
	}
	return parts[2], verb
}
