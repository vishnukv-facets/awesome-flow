package ghref

import "testing"

func TestPRTagFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "plain PR", url: "https://github.com/acme/app/pull/12", want: "gh-pr:acme/app#12"},
		{name: "review anchor", url: "https://github.com/acme/app/pull/12#pullrequestreview-44", want: "gh-pr:acme/app#12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PRTagFromURL(tt.url)
			if !ok {
				t.Fatalf("PRTagFromURL() ok = false")
			}
			if got != tt.want {
				t.Fatalf("tag = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPRTagFromURLRejectsNonPR(t *testing.T) {
	if tag, ok := PRTagFromURL("https://github.com/acme/app/issues/12"); ok {
		t.Fatalf("PRTagFromURL() = %q, true; want false", tag)
	}
}

func TestRepoFromRemote(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{name: "scp with user and .git", remote: "git@github.com:vishnukv-facets/flow-manager.git", want: "vishnukv-facets/flow-manager"},
		{name: "scp without .git", remote: "git@github.com:owner/repo", want: "owner/repo"},
		{name: "scp without user", remote: "github.com:owner/repo.git", want: "owner/repo"},
		{name: "https with .git", remote: "https://github.com/Facets-cloud/flow.git", want: "facets-cloud/flow"},
		{name: "https without .git", remote: "https://github.com/Facets-cloud/flow", want: "facets-cloud/flow"},
		{name: "ssh url", remote: "ssh://git@github.com/owner/Repo.git", want: "owner/repo"},
		{name: "https with extra path", remote: "https://github.com/owner/repo/", want: "owner/repo"},
		{name: "trailing whitespace", remote: "  git@github.com:owner/repo.git\n", want: "owner/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RepoFromRemote(tt.remote)
			if !ok {
				t.Fatalf("RepoFromRemote(%q) ok = false", tt.remote)
			}
			if got != tt.want {
				t.Fatalf("RepoFromRemote(%q) = %q, want %q", tt.remote, got, tt.want)
			}
		})
	}
}

func TestRepoFromRemoteRejects(t *testing.T) {
	cases := []string{
		"",                                  // empty
		"not a url",                         // garbage
		"git@gitlab.com:owner/repo.git",     // non-github host
		"https://gitlab.com/owner/repo.git", // non-github host
		"https://github.com/owneronly",      // missing repo segment
		"git@github.com:owneronly",          // missing repo segment
	}
	for _, c := range cases {
		if got, ok := RepoFromRemote(c); ok {
			t.Fatalf("RepoFromRemote(%q) = %q, true; want false", c, got)
		}
	}
}
