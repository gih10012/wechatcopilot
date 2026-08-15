package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/gih10012/wechatcopilot/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Execute(ctx, version))
}
