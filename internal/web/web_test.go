package web

import "testing"

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/s/areas/phial":        "/s/areas/phial",
		"/search?q=a%20b":       "/search?q=a%20b",
		"":                      "/",
		"https://evil.example/": "/",
		"//evil.example":        "/",
		"/\t/evil.example":      "/",
		"/\n/evil.example":      "/",
		`/\evil.example`:        "/",
		"/%09/evil.example":     "/%09/evil.example", // still encoded: the browser does not decode it into the path's start
		"javascript:alert(1)":   "/",
		"/ok\x7f":               "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
