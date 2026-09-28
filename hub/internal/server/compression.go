package server

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gvinsot/SwiftProof/hub/internal/store"
)

// Compress only the repository listing, never SSE or authentication responses.
func writeRepoList(w http.ResponseWriter, r *http.Request, repos []store.PublicRepo) {
	w.Header().Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r.Header.Values("Accept-Encoding")) {
		writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "recent_limit": store.MaxRecent})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	zipped := gzip.NewWriter(w)
	defer zipped.Close()
	_ = json.NewEncoder(zipped).Encode(map[string]any{"repos": repos, "recent_limit": store.MaxRecent})
}

func acceptsGzip(headers []string) bool {
	wildcard := false
	for _, header := range headers {
		for _, item := range strings.Split(header, ",") {
			parts := strings.Split(item, ";")
			coding := strings.TrimSpace(parts[0])
			quality := 1.0
			for _, param := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
				if ok && strings.EqualFold(key, "q") {
					parsed, err := strconv.ParseFloat(value, 64)
					if err != nil || parsed < 0 || parsed > 1 {
						quality = 0
					} else {
						quality = parsed
					}
				}
			}
			if strings.EqualFold(coding, "gzip") {
				return quality > 0
			}
			if coding == "*" {
				wildcard = quality > 0
			}
		}
	}
	return wildcard
}
