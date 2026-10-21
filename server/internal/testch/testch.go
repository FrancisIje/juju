// Package testch starts a real ClickHouse for integration tests, the way
// embedded-postgres does for Postgres: tests run the same queries
// production does, against the same database engine.
package testch

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// Start runs ClickHouse with scripts/devch.sh on ports base, base+1, +2, +3
// (HTTP, native, MySQL, PostgreSQL), so test packages running in parallel
// each get their own. It returns the HTTP URL and a stop function.
// JUJU_TEST_CLICKHOUSE_URL, if set, is used instead (one already running).
func Start(base int) (string, func(), error) {
	if u := os.Getenv("JUJU_TEST_CLICKHOUSE_URL"); u != "" {
		return u, func() {}, wait(u)
	}
	_, here, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(here), "../../../scripts/devch.sh")
	dir, err := os.MkdirTemp("", "juju-ch-*")
	if err != nil {
		return "", nil, err
	}
	port := func(n int) string { return strconv.Itoa(base + n) }
	cmd := exec.Command("sh", script, dir)
	// No watchdog: clickhouse server otherwise forks the real server from a
	// watchdog process, and stopping only the one we started would leave the
	// server running.
	cmd.Env = append(os.Environ(), "CH_HTTP_PORT="+port(0), "CH_TCP_PORT="+port(1), "CH_MYSQL_PORT="+port(2), "CH_PG_PORT="+port(3), "CLICKHOUSE_WATCHDOG_ENABLE=0")
	cmd.Stdout, cmd.Stderr = nil, os.Stderr // nil: /dev/null, no pipe to wait on
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	stop := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) // the whole process group
		_ = cmd.Wait()
		os.RemoveAll(dir)
	}
	u := "http://127.0.0.1:" + port(0)
	if err := wait(u); err != nil {
		stop()
		return "", nil, err
	}
	return u, stop, nil
}

func wait(u string) error {
	deadline := time.Now().Add(120 * time.Second) // the first run downloads ClickHouse
	for {
		if res, err := http.Get(u + "/ping"); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clickhouse did not answer at %s", u)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
