package api

import (
	"net/http"
	"strings"
)

// discoveryVersions mirrors the Image API version document captured from the
// pinned original (evidence/sprint2/glance/versions.response.json). Clients such
// as keystoneauth/glanceclient inside Nova use it to select major version 2; the
// minor entries are informational and do not widen the implemented route set.
var discoveryVersions = []struct{ id, status string }{
	{"v2.18", "CURRENT"}, {"v2.17", "SUPPORTED"}, {"v2.16", "SUPPORTED"}, {"v2.15", "SUPPORTED"},
	{"v2.14", "SUPPORTED"}, {"v2.9", "SUPPORTED"}, {"v2.7", "SUPPORTED"}, {"v2.6", "SUPPORTED"},
	{"v2.5", "SUPPORTED"}, {"v2.4", "SUPPORTED"}, {"v2.3", "SUPPORTED"}, {"v2.2", "SUPPORTED"},
	{"v2.1", "SUPPORTED"}, {"v2.0", "SUPPORTED"},
}

type versionLink struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

type versionEntry struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Links  []versionLink `json:"links"`
}

// discovery reports whether r is an unauthenticated version-discovery request
// and its status: the root answers 300 Multiple Choices and /versions 200, as
// in the original. Other methods on these paths keep the authenticated path.
func discovery(r *http.Request) (int, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return 0, false
	}
	switch r.URL.Path {
	case "/":
		return http.StatusMultipleChoices, true
	case "/versions":
		return http.StatusOK, true
	}
	return 0, false
}

func (h *Handler) versions(w http.ResponseWriter, status int) {
	href := strings.TrimRight(h.publicURL.String(), "/") + "/v2/"
	entries := make([]versionEntry, 0, len(discoveryVersions))
	for _, v := range discoveryVersions {
		entries = append(entries, versionEntry{ID: v.id, Status: v.status, Links: []versionLink{{Rel: "self", Href: href}}})
	}
	h.json(w, status, map[string][]versionEntry{"versions": entries})
}
