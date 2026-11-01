package main

import "testing"

// A CI checkout's remote often holds a token. Whatever the remote looks
// like, what's sent to the server must be a plain https URL without it.
func TestRepoURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"git@github.com:acme/web.git", "https://github.com/acme/web"},
		{"ssh://git@gitlab.com:2222/acme/web.git", "https://gitlab.com/acme/web"},
		{"https://x-access-token:ghs_abc123@github.com/acme/web.git", "https://github.com/acme/web"},
		{"https://oauth2:glpat-xyz@gitlab.com/acme/web", "https://gitlab.com/acme/web"},
		{"https://github.com/acme/web", "https://github.com/acme/web"},
		{"http://git.internal/acme/web.git", "https://git.internal/acme/web"},
		{"/Users/me/web", ""}, // a local path isn't a repository anyone can open
		{"", ""},
	}
	for _, tc := range tests {
		if got := repoURL(tc.in); got != tc.want {
			t.Errorf("repoURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
