package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("abc"), 0o644)
	got, err := fileSHA256(context.Background(), p)
	if err != nil || got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sha256: %s %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fileSHA256(ctx, p); err == nil {
		t.Fatal("a cancelled context must stop hashing")
	}
}
