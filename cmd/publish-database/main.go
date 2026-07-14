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
	"time"

	fetchv1 "github.com/TecharoHQ/reputationdb/gen/techaro/lol/reputationdb/fetch/v1"
	"github.com/facebookgo/flagenv"
	simplestorage "github.com/tigrisdata/storage-go/simplestorage"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		return fmt.Errorf("reading database %s: %w", dbPath, err)
	}

	id := versionID(raw)
	key := objectKey(id)
	lg.Info("read database", "path", dbPath, "bytes", len(raw), "version_id", id)

	// Read provenance before uploading anything: a repo we can't read is fatal,
	// and failing fast avoids leaving an unreferenced object in the bucket.
	info, err := repoMetadata(lg, dbPath)
	if err != nil {
		return err
	}

	compressed, err := compressDatabase(raw)
	if err != nil {
		return err
	}
	lg.Info("compressed database", "bytes", len(compressed), "ratio", fmt.Sprintf("%.2f", float64(len(compressed))/float64(len(raw))))

	st, err := simplestorage.New(ctx, simplestorage.WithBucket(*tigrisBucket))
	if err != nil {
		return fmt.Errorf("creating Tigris client: %w", err)
	}

	idx, err := loadIndex(ctx, st, lg)
	if err != nil {
		return err
	}

	if err := putDatabase(ctx, st, key, compressed); err != nil {
		return err
	}
	lg.Info("uploaded database", "bucket", *tigrisBucket, "key", key)

	// The index is written only after the object it points at exists, so a
	// failed upload can never leave the index advertising a missing database.
	kept, evicted := insertVersion(idx.GetVersions(), &fetchv1.DatabaseVersion{
		CreatedAt:         timestamppb.New(time.Now().UTC()),
		RepoShasum:        info.Shasum,
		RepoCommitMessage: info.Message,
		VersionId:         id,
	})

	if err := saveIndex(ctx, st, &fetchv1.ListResponse{Versions: kept}); err != nil {
		return err
	}

	for _, v := range evicted {
		// The objects stay in the bucket so clients holding an older version ID
		// can still fetch them; only the index forgets about them.
		lg.Info("version aged out of the index; its object was left in place",
			"version_id", v.GetVersionId(), "key", objectKey(v.GetVersionId()))
	}

	lg.Info("published database",
		"version_id", id,
		"key", key,
		"shasum", info.Shasum,
		"versions_in_index", len(kept),
	)

	return nil
}
