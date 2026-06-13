package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sihuo/codex-feishu-bridge/internal/doctor"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	return runWithIO(ctx, args, os.Stdout, os.Stderr)
}

func runWithIO(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: codex-feishu-bridge <serve|init-config|doctor|tasks list|tasks show>")
		return 2
	}
	switch args[0] {
	case "doctor":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		report := doctor.Check(ctx, doctor.Options{ConfigPath: *configPath})
		fmt.Fprint(stdout, report.Render())
		if report.HasErrors() {
			return 1
		}
		return 0
	case "serve", "init-config":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		_ = ctx
		_ = configPath
		fmt.Fprintf(stdout, "%s is not implemented yet\n", args[0])
		return 1
	case "tasks":
		fmt.Fprintln(stdout, "tasks list/show is not implemented yet")
		return 1
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}
