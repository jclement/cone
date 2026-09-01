package inbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenSource is the three ways a declared service can carry its bearer token: inline, from an
// environment variable, or from a KEY=VALUE credentials file. It is one type, embedded by every
// config cone reads (inboxes.json and human.json), so the semantics cannot drift between them —
// an enrollment file that works for an inbox works identically for the human service.
type TokenSource struct {
	Token     string `json:"token"`
	TokenEnv  string `json:"token_env"`
	TokenFile string `json:"token_file"`
	TokenKey  string `json:"token_key"`
}

// Resolve returns the bearer token, trying the three sources in order of directness. An empty
// result with no error means none was configured; callers decide whether that is fatal.
func (ts TokenSource) Resolve() (string, error) {
	if ts.Token != "" {
		return ts.Token, nil
	}
	if ts.TokenEnv != "" {
		return os.Getenv(ts.TokenEnv), nil
	}
	if ts.TokenFile == "" {
		return "", nil
	}
	path := ts.TokenFile
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	// A KEY=VALUE file, which is what every service's enrollment writes. Without a token_key
	// the first value wins, so a single-line file needs no extra configuration.
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if ts.TokenKey == "" || strings.TrimSpace(k) == ts.TokenKey {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s has no %s", path, orDefault(ts.TokenKey, "usable KEY=VALUE line"))
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
