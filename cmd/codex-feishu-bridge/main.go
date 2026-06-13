package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: codex-feishu-bridge <serve|init-config|doctor|tasks list|tasks show>")
		return 2
	}
	switch args[0] {
	case "serve", "init-config", "doctor":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		configPath := fs.String("config", "", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		_ = ctx
		_ = configPath
		fmt.Fprintf(os.Stdout, "%s is not implemented yet\n", args[0])
		return 1
	case "tasks":
		fmt.Fprintln(os.Stdout, "tasks list/show is not implemented yet")
		return 1
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
}
