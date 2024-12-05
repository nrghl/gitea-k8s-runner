// main.go

package main

import (
	"context"
	"os/signal"
	"syscall"

	"gitea.com/gitea/act_runner/internal/app/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// run the command
	cmd.Execute(ctx)
}
