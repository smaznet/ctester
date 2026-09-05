package install

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// Options for Install.
type Options struct {
	ConfigPath string // source config to copy (required if dest missing)
	BinPath    string // install binary here (default /usr/local/bin/x-tester)
	Dir        string // config/data dir (default /etc/x-tester)
	UnitName   string // systemd unit name without .service (default x-tester)
	User       string // optional systemd User=
	NoStart    bool
	NoEnable   bool
	ForceCfg   bool // overwrite existing config
}

const unitTemplate = `[Unit]
Description=x-tester — Xray probe / sticky balancer
Documentation=https://github.com/smaznet/ctester
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
{{if .User}}User={{.User}}
{{end}}WorkingDirectory={{.Dir}}
ExecStart={{.BinPath}} -c {{.ConfigDest}}
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

# Logs go to journal (headless when no TTY).
StandardOutput=journal
StandardError=journal
SyslogIdentifier=x-tester

[Install]
WantedBy=multi-user.target
`

// Install copies the current binary + config and writes a systemd unit.
func Install(opts Options) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("install is only supported on Linux (systemd)")
	}
	if opts.BinPath == "" {
		opts.BinPath = "/usr/local/bin/x-tester"
	}
	if opts.Dir == "" {
		opts.Dir = "/etc/x-tester"
	}
	if opts.UnitName == "" {
		opts.UnitName = "x-tester"
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = "config.yaml"
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", opts.Dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(opts.BinPath), 0o755); err != nil {
		return fmt.Errorf("mkdir bin: %w", err)
	}

	fmt.Printf("installing binary → %s\n", opts.BinPath)
	if err := copyFile(exe, opts.BinPath, 0o755); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	cfgDest := filepath.Join(opts.Dir, "config.yaml")
	if _, err := os.Stat(cfgDest); err == nil && !opts.ForceCfg {
		fmt.Printf("keeping existing config %s (use -force-config to overwrite)\n", cfgDest)
	} else {
		src := opts.ConfigPath
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("config %s: %w (pass -c path/to/config.yaml)", src, err)
		}
		fmt.Printf("installing config → %s\n", cfgDest)
		if err := copyFile(src, cfgDest, 0o644); err != nil {
			return fmt.Errorf("copy config: %w", err)
		}
	}

	unitPath := filepath.Join("/etc/systemd/system", opts.UnitName+".service")
	fmt.Printf("writing unit → %s\n", unitPath)
	data := struct {
		User       string
		Dir        string
		BinPath    string
		ConfigDest string
	}{
		User:       opts.User,
		Dir:        opts.Dir,
		BinPath:    opts.BinPath,
		ConfigDest: cfgDest,
	}
	tmpl, err := template.New("unit").Parse(unitTemplate)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(unitPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("write unit (need root?): %w", err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if !opts.NoEnable {
		if err := run("systemctl", "enable", opts.UnitName+".service"); err != nil {
			return err
		}
	}
	if !opts.NoStart {
		if err := run("systemctl", "restart", opts.UnitName+".service"); err != nil {
			return err
		}
		_ = run("systemctl", "--no-pager", "--full", "status", opts.UnitName+".service")
	}

	fmt.Printf("\ninstalled. useful commands:\n")
	fmt.Printf("  systemctl status %s\n", opts.UnitName)
	fmt.Printf("  journalctl -u %s -f\n", opts.UnitName)
	fmt.Printf("  %s update && systemctl restart %s\n", opts.BinPath, opts.UnitName)
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
