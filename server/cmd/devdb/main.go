// Command devdb runs a throwaway Postgres for local development, without
// Docker. It downloads real Postgres binaries once (cached in
// ~/.embedded-postgres-go) and listens on the same port and credentials as
// docker-compose.yml, so `juju serve` works the same either way.
//
// Data lives in the -dir folder (`make devdb` uses .devdb/ at the repo root)
// and survives restarts; delete the folder for a clean slate.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func main() {
	dir := flag.String("dir", ".devdb", "where to keep the database files")
	flag.Parse()
	dataDir, err := filepath.Abs(*dir)
	if err != nil {
		fail(err)
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).
		Port(5432).
		Username("juju").
		Password("juju").
		Database("juju").
		DataPath(filepath.Join(dataDir, "data")).
		RuntimePath(filepath.Join(dataDir, "runtime")).
		Logger(os.Stderr))
	if err := pg.Start(); err != nil {
		fail(err)
	}
	fmt.Println("postgres ready: postgres://juju:juju@localhost:5432/juju (Ctrl-C to stop)")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	if err := pg.Stop(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "devdb:", err)
	os.Exit(1)
}
