package xray_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aria/x-tester/internal/xray"
)

func TestAddRemoveNode(t *testing.T) {
	if _, err := exec.LookPath("xray"); err != nil {
		t.Skip("xray not in PATH")
	}
	dir := t.TempDir()
	inst, err := xray.StartProbe("xray", filepath.Join(dir, "p"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	local, err := inst.AddNode(ctx, "t", map[string]any{
		"protocol": "freedom",
		"settings": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if local.LocalPort <= 0 {
		t.Fatalf("bad port: %+v", local)
	}
	if err := inst.RemoveNode(ctx, local.OutboundTag, local.InboundTag); err != nil {
		t.Fatal(err)
	}
	_ = os.Stderr
}
