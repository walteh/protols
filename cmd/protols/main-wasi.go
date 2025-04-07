//go:build wasip1

package main

import (
	"context"
	"os"

	"github.com/kralicky/protols/pkg/wasi"
)

func main() {
	ctx := context.Background()

	if err := wasi.Serve(ctx); err != nil {
		os.Exit(1)
	}
	select {}
}
