package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const serviceName = "News-Proxy"

func main() {
	if isWindowsService() {
		runWindowsService()
		return
	}

	cmd, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "version":
		fmt.Println("News-Proxy", version)
	case "run":
		runConsole(args)
	default:
		if platformCommand(cmd, args) {
			return
		}
		if cmd == "" {
			runConsole(args)
			return
		}
		fmt.Fprintf(os.Stderr, "Unknown command %q.\nUsage: news-proxy [run] [--data DIR]\n", cmd)
		os.Exit(2)
	}
}

func resolveDataDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("DATA_DIR"); env != "" {
		return env
	}
	return defaultDataDir()
}

func runConsole(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	data := fs.String("data", "", "data directory (settings, sources, cache)")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, resolveDataDir(*data)); err != nil {
		log.Fatalf("news-proxy: %v", err)
	}
}
