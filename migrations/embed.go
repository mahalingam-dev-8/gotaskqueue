// Package migrations embeds the .sql files next to this file into the binary.
//
// Go idiom: //go:embed bakes files into the compiled binary at build time, so the
// final Docker image is a single static file with no migrations/ directory to ship.
// The embed.FS is a read-only filesystem you can walk like any other fs.FS.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
