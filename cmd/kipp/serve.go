package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"time"

	"github.com/uhthomas/kipp"
	"github.com/uhthomas/kipp/database/badger"
	"github.com/uhthomas/kipp/internal/filesystemutil"
)

func serve(ctx context.Context) error {
	addr := flag.String("addr", ":80", "listen addr")
	dsn := flag.String("dsn", "badger", "data source name")
	fsf := flag.String("filesystem", "files", "filesystem - see docs for more information")
	web := flag.String("web", "web", "web directory")
	limit := flagBytesValue("limit", 150<<20, "upload limit")
	lifetime := flag.Duration("lifetime", 24*time.Hour, "file lifetime")
	flag.Parse()

	for k, v := range mimeTypes {
		for _, vv := range v {
			if err := mime.AddExtensionType(vv, k); err != nil {
				return fmt.Errorf("add mime extension type: %w", err)
			}
		}
	}

	// s3://some-key:some-token@some-region/some-bucket
	// b2://some-token@some-region/some-bucket
	// gcp://some-token@some-region/some-bucket

	fs, err := filesystemutil.Parse(*fsf)
	if err != nil {
		return fmt.Errorf("parse filesystem: %w", err)
	}

	db, err := badger.Open(*dsn)
	if err != nil {
		return fmt.Errorf("open badger: %w", err)
	}
	defer db.Close(ctx)

	log.Printf("listening on %s", *addr)

	return (&http.Server{
		Addr: *addr,
		Handler: &kipp.Server{
			Database:   db,
			FileSystem: fs,
			Limit:      int64(*limit),
			Lifetime:   *lifetime,
			PublicPath: *web,
		},
		// ReadTimeout:  5 * time.Second,
		// WriteTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}).ListenAndServe()
}
