package ghref

import (
	"net/url"
	"strconv"
	"strings"
)

func PRTagFromURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(u.Host, "github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", false
	}
	return "gh-pr:" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1]) + "#" + strconv.Itoa(n), true
}

// RepoFromRemote normalizes a git "origin" remote URL into a lowercase
// "owner/repo" GitHub slug. It accepts the scp-like form
// (git@github.com:owner/repo.git), https/ssh URLs
// (https://github.com/owner/repo[.git], ssh://git@github.com/owner/repo),
// with or without a trailing ".git" or a user prefix.
//
// Only github.com remotes match. GitHub monitor events are always
// github.com, and restricting the host prevents a coincidental same-named
// repo on another host (gitlab, etc.) from matching a project. Returns
// ok=false for anything that is not a github.com remote carrying both an
// owner and a repo.
func RepoFromRemote(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	var host, path string
	if strings.Contains(s, "://") {
		// scheme://[user@]host[:port]/owner/repo(.git)
		u, err := url.Parse(s)
		if err != nil {
			return "", false
		}
		host = u.Host
		path = u.Path
	} else if strings.Contains(s, ":") {
		// scp-like: [user@]host:owner/repo(.git)
		hostpart := s
		if at := strings.LastIndex(hostpart, "@"); at >= 0 {
			hostpart = hostpart[at+1:]
		}
		var ok bool
		host, path, ok = strings.Cut(hostpart, ":")
		if !ok {
			return "", false
		}
	} else {
		return "", false
	}
	// url.Host may still carry userinfo / a port; strip both.
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	if !strings.EqualFold(host, "github.com") {
		return "", false
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), true
}
