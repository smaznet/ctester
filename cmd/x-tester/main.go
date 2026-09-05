package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aria/x-tester/internal/app"
	"github.com/aria/x-tester/internal/install"
	"github.com/aria/x-tester/internal/monitor"
	"github.com/aria/x-tester/internal/selfupdate"
	"github.com/aria/x-tester/internal/version"
)

func main() {
	if len(os.Args) >= 2 && !isFlag(os.Args[1]) {
		switch os.Args[1] {
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "install":
			os.Exit(runInstall(os.Args[2:]))
		case "monitor":
			os.Exit(runMonitor(os.Args[2:]))
		case "version":
			fmt.Println(version.String())
			return
		case "help", "-h", "--help":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "x-tester: unknown command %q\n\n", os.Args[1])
			printUsage()
			os.Exit(2)
		}
	}

	fs := flag.NewFlagSet("x-tester", flag.ExitOnError)
	cfgPath := fs.String("c", "config.yaml", "path to config yaml")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = printUsage
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version.String())
		return
	}
	if err := app.Run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "x-tester: %v\n", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	repo := fs.String("repo", "smaznet/ctester", "GitHub owner/repo")
	tag := fs.String("tag", "latest", "release tag to install")
	force := fs.Bool("force", false, "replace even if checksum matches")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: x-tester update [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Download the release binary from GitHub and replace this executable.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if err := selfupdate.Update(selfupdate.Options{
		Repo:  *repo,
		Tag:   *tag,
		Force: *force,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "x-tester update: %v\n", err)
		return 1
	}
	return 0
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	cfgPath := fs.String("c", "config.yaml", "config file to install under /etc/x-tester/")
	binPath := fs.String("bin", "/usr/local/bin/x-tester", "install binary path")
	dir := fs.String("dir", "/etc/x-tester", "config/data directory")
	unit := fs.String("unit", "x-tester", "systemd unit name (without .service)")
	user := fs.String("user", "", "optional systemd User=")
	noStart := fs.Bool("no-start", false, "do not start/restart the service")
	noEnable := fs.Bool("no-enable", false, "do not enable on boot")
	forceCfg := fs.Bool("force-config", false, "overwrite existing installed config")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: x-tester install [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Install binary + config and create a systemd service (Linux, root).\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if err := install.Install(install.Options{
		ConfigPath: *cfgPath,
		BinPath:    *binPath,
		Dir:        *dir,
		UnitName:   *unit,
		User:       *user,
		NoStart:    *noStart,
		NoEnable:   *noEnable,
		ForceCfg:   *forceCfg,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "x-tester install: %v\n", err)
		return 1
	}
	return 0
}

func runMonitor(args []string) int {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	cfgPath := fs.String("c", "config.yaml", "config used to resolve stats host/port")
	addr := fs.String("addr", "", "stats address host:port (overrides -c)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: x-tester monitor [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Attach TUI to a running x-tester (e.g. systemd) via its stats HTTP API.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if err := monitor.Run(monitor.Options{
		Addr:       *addr,
		ConfigPath: *cfgPath,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "x-tester monitor: %v\n", err)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `x-tester %s — Xray probe / sticky balancer

Usage:
  x-tester -c config.yaml          Run (TUI if tty, otherwise headless)
  x-tester monitor [flags]         Attach TUI to a running instance
  x-tester update [flags]          Download latest binary from GitHub
  x-tester install [flags]         Install systemd service (Linux)
  x-tester version                 Print version

Run flags:
  -c string
        path to config yaml (default "config.yaml")
  -version
        print version and exit

Monitor flags:
  -c string      config to read stats.host/port (default "config.yaml")
  -addr string   stats address host:port (overrides -c)

Update flags:
  -repo string   GitHub owner/repo (default "smaznet/ctester")
  -tag string    release tag (default "latest")
  -force         replace even if already current

Install flags:
  -c string            config to copy (default "config.yaml")
  -bin string          binary install path (default "/usr/local/bin/x-tester")
  -dir string          config/data dir (default "/etc/x-tester")
  -unit string         systemd unit name (default "x-tester")
  -user string         optional systemd User=
  -no-start            skip systemctl restart
  -no-enable           skip systemctl enable
  -force-config        overwrite existing /etc/x-tester/config.yaml

`, version.String())
}
