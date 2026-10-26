package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// release records which git commit a release was built from, so an agent
// (or a person) fixing a crash reads the code that crashed:
//
//	juju release -name web@2.3.1                 commit and repository from git
//	juju release -name web@2.3.1 -commit abc1234 -repo https://github.com/acme/web
//
// Run it in CI next to `juju upload`. Like upload, it needs no database:
// it calls the server with JUJU_URL, JUJU_PROJECT and JUJU_SECRET_KEY.
func release(args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	server := fs.String("url", envOr("JUJU_URL", "http://localhost:8080"), "juju server URL (JUJU_URL)")
	project := fs.String("project", os.Getenv("JUJU_PROJECT"), "project id (JUJU_PROJECT)")
	secret := fs.String("secret", os.Getenv("JUJU_SECRET_KEY"), "project secret key (JUJU_SECRET_KEY)")
	name := fs.String("name", "", "the release, exactly as the SDK sends it, e.g. web@2.3.1")
	commit := fs.String("commit", "", "git commit id (default: git rev-parse HEAD)")
	repo := fs.String("repo", "", "repository URL (default: the origin remote, as an https URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *secret == "" {
		return errors.New("set -project and -secret (or JUJU_PROJECT and JUJU_SECRET_KEY)")
	}
	if *name == "" {
		return errors.New("set -name to the release your SDK sends")
	}
	if *commit == "" {
		out, err := git("rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("no -commit given and git couldn't tell: %w", err)
		}
		*commit = out
	}
	if *repo == "" {
		// Optional: without a remote the commit alone still helps.
		if out, err := git("remote", "get-url", "origin"); err == nil {
			*repo = repoURL(out)
		}
	}

	body, _ := json.Marshal(map[string]string{"name": *name, "commit": *commit, "repository": *repo})
	endpoint := strings.TrimRight(*server, "/") + "/api/v1/projects/" + url.PathEscape(*project) + "/releases"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	resBody, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(resBody, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", res.Status, e.Error)
		}
		return fmt.Errorf("%s", res.Status)
	}
	fmt.Printf("✓ release %s → commit %s", *name, *commit)
	if *repo != "" {
		fmt.Printf(" in %s", *repo)
	}
	fmt.Println()
	return nil
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// repoURL turns a git remote into a browsable https URL and drops any
// credentials in it. CI checkouts often have a token in the remote
// (https://x-access-token:ghs_…@github.com/acme/web.git), which must never
// be sent to the server.
//
//	git@github.com:acme/web.git           → https://github.com/acme/web
//	ssh://git@gitlab.com/acme/web.git     → https://gitlab.com/acme/web
//	https://user:token@github.com/acme/web → https://github.com/acme/web
func repoURL(remote string) string {
	remote = strings.TrimSpace(remote)
	if host, path, ok := strings.Cut(remote, ":"); ok && !strings.Contains(remote, "://") && strings.Contains(host, "@") {
		// scp-like syntax: user@host:path
		remote = "https://" + host[strings.LastIndex(host, "@")+1:] + "/" + path
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return ""
	}
	u.Scheme, u.User = "https", nil
	u.Host = u.Hostname() // an ssh port isn't the web port
	u.Path = strings.TrimSuffix(u.Path, ".git")
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}
