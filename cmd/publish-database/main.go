// Command publish-database uploads a reputation database to Tigris and
// maintains the index of recent database versions.
//
// The database named by the sole positional argument is uploaded as a private,
// zstd-compressed object keyed by the base64url SHA-512 of its contents. The
// index object at the bucket root is then updated to describe the ten most
// recent versions:
//
//	publish-database ./var/reputationdb.mmdb
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/facebookgo/flagenv"

	_ "github.com/joho/godotenv/autoload"
)

var (
	tigrisBucket = flag.String("tigris-storage-bucket", "techaro-reputationdb", "Tigris bucket the reputation DB should be stored in")
)

func main() {
	flagenv.Parse()
	flag.Parse()

	lg := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("program", "publish-database")
	slog.SetDefault(lg)

	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <database.mmdb>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := run(ctx, lg, args[0]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, lg *slog.Logger, dbPath string) error {
	return nil
}
