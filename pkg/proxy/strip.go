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
	for seg := range strings.SplitSeq(strings.TrimPrefix(clean, "/"), "/") {
		switch seg {
		case "exec", "attach", "portforward", "proxy":
			return true
		}
	}
	return false
}
