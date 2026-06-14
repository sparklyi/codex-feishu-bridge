package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sparklyi/codex-feishu-bridge/internal/app"
	"github.com/sparklyi/codex-feishu-bridge/internal/doctor"
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
	case "init-config":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "config file path")
		force := fs.Bool("force", false, "overwrite existing config")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := app.InitConfig(*configPath, *force); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "config initialized")
		return 0
	case "serve":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if err := app.Serve(ctx, app.ServeOptions{ConfigPath: *configPath}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "tasks":
		return runTasks(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func runTasks(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: codex-feishu-bridge tasks <list|show>")
		return 2
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		tasks, err := app.ListTasks(ctx, *configPath, os.Getenv, 50)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, task := range tasks {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", task.ID, task.Status, task.ProjectAlias)
		}
		return 0
	case "show":
		fs := flag.NewFlagSet("tasks show", flag.ContinueOnError)
		fs.SetOutput(stderr)
		configPath := fs.String("config", "", "config file path")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		rest := fs.Args()
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: codex-feishu-bridge tasks show [--config path] <task_id>")
			return 2
		}
		task, runs, err := app.ShowTask(ctx, *configPath, os.Getenv, rest[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "task %s\nstatus %s\nproject %s\ncwd %s\nruns %d\n", task.ID, task.Status, task.ProjectAlias, task.CWD, len(runs))
		return 0
	default:
		fmt.Fprintf(stderr, "unknown tasks command %q\n", args[0])
		return 2
	}
}
