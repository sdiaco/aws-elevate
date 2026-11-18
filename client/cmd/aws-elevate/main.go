// Command aws-elevate is the client of aws-elevate.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/sdiaco/aws-elevate/client/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	env, err := cli.DefaultEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "aws-elevate: %v\n", err)
		os.Exit(1)
	}
	os.Exit(cli.Run(ctx, env, os.Args[1:]))
}
