package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const slotCount = 512

type Instance struct {
	Name    string
	Bin     string
	APIAddr string
	WorkDir string
	cmd     *exec.Cmd
	mu      sync.Mutex // process + slots
	apiMu   sync.Mutex // serialize HandlerService calls
	slots   []bool
}

func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func StartMain(bin, workDir string) (*Instance, error) {
	apiPort, err := FreePort()
	if err != nil {
		return nil, err
	}
	return start(bin, workDir, "main", apiPort, baseConfig(apiPort))
}

func StartProbe(bin, workDir string) (*Instance, error) {
	apiPort, err := FreePort()
	if err != nil {
		return nil, err
	}
	return start(bin, workDir, "probe", apiPort, baseConfig(apiPort))
}

func baseConfig(apiPort int) map[string]any {
	rules := make([]any, 0, slotCount)
	for i := 0; i < slotCount; i++ {
		rules = append(rules, map[string]any{
			"type":        "field",
			"inboundTag":  []string{fmt.Sprintf("slot-in-%d", i)},
			"outboundTag": fmt.Sprintf("slot-out-%d", i),
		})
	}
	return map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{
			"tag":      "api",
			"listen":   fmt.Sprintf("127.0.0.1:%d", apiPort),
			"services": []string{"HandlerService"},
		},
		"inbounds": []any{},
		"outbounds": []any{
			map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
			map[string]any{"tag": "block", "protocol": "blackhole", "settings": map[string]any{}},
		},
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules":          rules,
		},
	}
}

func start(bin, workDir, name string, apiPort int, cfg map[string]any) (*Instance, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(workDir, name+"-config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "run", "-c", cfgPath)
	cmd.Dir = workDir
	logFile, err := os.Create(filepath.Join(workDir, name+".log"))
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start xray %s: %w", name, err)
	}
	inst := &Instance{
		Name:    name,
		Bin:     bin,
		APIAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(apiPort)),
		WorkDir: workDir,
		cmd:     cmd,
		slots:   make([]bool, slotCount),
	}
	if err := inst.waitAPI(10 * time.Second); err != nil {
		_ = inst.Stop()
		return nil, err
	}
	return inst, nil
}

func (i *Instance) waitAPI(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", i.APIAddr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("xray %s api %s not ready", i.Name, i.APIAddr)
}

func (i *Instance) Stop() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cmd == nil || i.cmd.Process == nil {
		return nil
	}
	_ = i.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- i.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		return i.cmd.Process.Kill()
	}
}

func (i *Instance) apiCmdLocked(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("apiCmd: empty args")
	}
	cmdName := args[0]
	rest := args[1:]
	full := []string{"api", cmdName, "--server=" + i.APIAddr}
	full = append(full, rest...)
	cmd := exec.CommandContext(ctx, i.Bin, full...)
	cmd.Dir = i.WorkDir
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", compactAPIErr(cmdName, msg))
	}
	return nil
}

func compactAPIErr(cmd, msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " | ")
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	return fmt.Sprintf("%s: %s", cmd, msg)
}

func (i *Instance) writeJSON(name string, v any) (string, error) {
	path := filepath.Join(i.WorkDir, name)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (i *Instance) allocSlot() (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for idx, used := range i.slots {
		if !used {
			i.slots[idx] = true
			return idx, nil
		}
	}
	return 0, fmt.Errorf("no free xray slots (max %d) — lower probe.concurrency or wait for removals", slotCount)
}

func (i *Instance) freeSlot(idx int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if idx >= 0 && idx < len(i.slots) {
		i.slots[idx] = false
	}
}

type NodeLocal struct {
	Tag         string
	OutboundTag string
	InboundTag  string
	LocalPort   int
	Slot        int
}

func (i *Instance) AddNode(ctx context.Context, _ string, outbound map[string]any) (*NodeLocal, error) {
	slot, err := i.allocSlot()
	if err != nil {
		return nil, err
	}
	port, err := FreePort()
	if err != nil {
		i.freeSlot(slot)
		return nil, err
	}
	outTag := fmt.Sprintf("slot-out-%d", slot)
	inTag := fmt.Sprintf("slot-in-%d", slot)

	out := cloneMap(outbound)
	out["tag"] = outTag
	proto, _ := out["protocol"].(string)

	outPath, err := i.writeJSON(outTag+".json", map[string]any{"outbounds": []any{out}})
	if err != nil {
		i.freeSlot(slot)
		return nil, err
	}

	i.apiMu.Lock()
	defer i.apiMu.Unlock()

	if err := i.apiCmdLocked(ctx, "ado", outPath); err != nil {
		// stale tag left behind — force remove and retry once
		_ = i.apiCmdLocked(ctx, "rmo", outTag)
		_ = i.apiCmdLocked(ctx, "rmi", inTag)
		if err2 := i.apiCmdLocked(ctx, "ado", outPath); err2 != nil {
			_ = i.saveLastFail(outTag, proto, outPath, err2)
			i.freeSlot(slot)
			return nil, fmt.Errorf("add outbound tag=%s proto=%s file=%s: %w", outTag, proto, filepath.Base(outPath), err2)
		}
	}

	inbound := map[string]any{
		"tag":      inTag,
		"listen":   "127.0.0.1",
		"port":     port,
		"protocol": "socks",
		"settings": map[string]any{
			"udp":  true,
			"auth": "noauth",
		},
	}
	inPath, err := i.writeJSON(inTag+".json", map[string]any{"inbounds": []any{inbound}})
	if err != nil {
		_ = i.apiCmdLocked(ctx, "rmo", outTag)
		i.freeSlot(slot)
		return nil, err
	}
	if err := i.apiCmdLocked(ctx, "adi", inPath); err != nil {
		_ = i.apiCmdLocked(ctx, "rmi", inTag)
		_ = i.apiCmdLocked(ctx, "rmo", outTag)
		_ = i.saveLastFail(inTag, "socks-in", inPath, err)
		i.freeSlot(slot)
		return nil, fmt.Errorf("add inbound tag=%s: %w", inTag, err)
	}

	return &NodeLocal{
		Tag:         outTag,
		OutboundTag: outTag,
		InboundTag:  inTag,
		LocalPort:   port,
		Slot:        slot,
	}, nil
}

func (i *Instance) saveLastFail(tag, proto, cfgPath string, err error) error {
	payload := map[string]any{
		"tag":      tag,
		"protocol": proto,
		"config":   cfgPath,
		"error":    err.Error(),
		"at":       time.Now().Format(time.RFC3339),
	}
	_, werr := i.writeJSON("last-api-fail.json", payload)
	return werr
}

func (i *Instance) RemoveNode(ctx context.Context, outboundTag, inboundTag string) error {
	i.apiMu.Lock()
	defer i.apiMu.Unlock()

	var first error
	if inboundTag != "" {
		if err := i.apiCmdLocked(ctx, "rmi", inboundTag); err != nil && first == nil {
			// ignore "not found"
			if !strings.Contains(strings.ToLower(err.Error()), "not found") {
				first = err
			}
		}
	}
	if outboundTag != "" {
		if err := i.apiCmdLocked(ctx, "rmo", outboundTag); err != nil && first == nil {
			if !strings.Contains(strings.ToLower(err.Error()), "not found") {
				first = err
			}
		}
	}
	var slot int
	if _, err := fmt.Sscanf(outboundTag, "slot-out-%d", &slot); err == nil {
		i.freeSlot(slot)
	}
	return first
}

func cloneMap(in map[string]any) map[string]any {
	b, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
