package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type discoveryDoc struct {
	Versions []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Links  []struct {
			Rel  string `json:"rel"`
			Href string `json:"href"`
		} `json:"links"`
	} `json:"versions"`
}

func decodeDiscovery(t *testing.T, body []byte) discoveryDoc {
	t.Helper()
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Nova's keystoneauth discovers the image endpoint before any call; without a
// public answer it fails with MissingAuthPlugin (HTTP 500 to the user).
func TestVersionDiscoveryIsPublicAndMatchesOriginal(t *testing.T) {
	h, _, a, logs := harness()
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/", 300}, {"GET", "/versions", 200}, {"HEAD", "/", 300}} {
		r := httptest.NewRequest(tc.method, tc.path, nil) // no X-Auth-Token
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s %s: status %d type %q", tc.method, tc.path, w.Code, w.Header().Get("Content-Type"))
		}
		if w.Header().Get("WWW-Authenticate") != "" || w.Header().Get("X-Openstack-Request-Id") == "" {
			t.Fatal("discovery must not challenge and must keep the request id")
		}
		if tc.method == "HEAD" {
			continue
		}
		doc := decodeDiscovery(t, w.Body.Bytes())
		if len(doc.Versions) != len(discoveryVersions) || doc.Versions[0].ID != "v2.18" || doc.Versions[0].Status != "CURRENT" {
			t.Fatalf("unexpected versions %+v", doc.Versions)
		}
		for _, v := range doc.Versions {
			if len(v.Links) != 1 || v.Links[0].Rel != "self" || v.Links[0].Href != "http://127.0.0.1:9293/v2/" {
				t.Fatalf("link must use the configured public URL: %+v", v.Links)
			}
		}
	}
	if a.calls != 0 {
		t.Fatal("discovery must not call Keystone")
	}
	if !strings.Contains(logs.String(), `"route":"/"`) || !strings.Contains(logs.String(), `"route":"/versions"`) {
		t.Fatal("discovery requests must be logged with their route")
	}

	raw, err := os.ReadFile("../../../evidence/sprint2/glance/versions.response.json")
	if err != nil {
		t.Fatal(err)
	}
	original := decodeDiscovery(t, raw)
	if len(original.Versions) != len(discoveryVersions) {
		t.Fatalf("original lists %d versions", len(original.Versions))
	}
	for i, v := range original.Versions {
		if v.ID != discoveryVersions[i].id || v.Status != discoveryVersions[i].status {
			t.Fatalf("version %d differs from the original: %s/%s", i, v.ID, v.Status)
		}
	}
}

func TestOtherRootMethodsStayAuthenticated(t *testing.T) {
	h, _, _, _ := harness()
	for _, path := range []string{"/", "/versions", "/v2/images"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("POST %s without token: %d", path, w.Code)
		}
	}
	if w := request(h, "GET", "/v2/unknown", ""); w.Code != 404 {
		t.Fatalf("unknown authenticated route: %d", w.Code)
	}
}
