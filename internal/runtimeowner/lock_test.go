//go:build darwin || linux

package runtimeowner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOwnershipCloseReleasesInheritedDescription(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	owner, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// Dup shares the same open file description, as a fork before exec does.
	// Keep it open past Close to exercise release independently of child exit.
	inherited, err := unix.Dup(int(owner.file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(inherited)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(path)
	if err != nil {
		t.Fatalf("closed owner retained by inherited descriptor: %v", err)
	}
	defer next.Close()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if third, err := Acquire(path); !errors.Is(err, ErrOwned) {
		if third != nil {
			_ = third.Close()
		}
		t.Fatalf("repeated close released the next owner's lock: %v", err)
	}
}

func TestOwnershipExcludesOtherProcessAndReleasesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	owner, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	other, err := Acquire(path)
	if other != nil {
		other.Close()
	}
	if !errors.Is(err, ErrOwned) {
		t.Fatalf("second owner: %v", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestOwnershipChild$")
	command.Env = append(os.Environ(), "GSEA_OWNERSHIP_TEST_PATH="+path)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child=%s err=%v", out, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
}

func TestOwnershipChild(t *testing.T) {
	path := os.Getenv("GSEA_OWNERSHIP_TEST_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	owner, err := Acquire(path)
	if owner != nil {
		owner.Close()
	}
	if !errors.Is(err, ErrOwned) {
		t.Fatalf("cross-process lock failed: %v", err)
	}
}

func TestOwnershipCanonicalizesDirectoryAliases(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "data")
	owner, err := Acquire(filepath.Join(directory, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	other, err := Acquire(filepath.Join(alias, "sessions.db"))
	if other != nil {
		other.Close()
	}
	if !errors.Is(err, ErrOwned) {
		t.Fatalf("alias admitted: %v", err)
	}
}

func TestOwnershipRejectsDanglingDatabaseAlias(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(filepath.Join(root, "not-created.db"), alias); err != nil {
		t.Fatal(err)
	}
	owner, err := Acquire(alias)
	if owner != nil {
		_ = owner.Close()
	}
	if err == nil {
		t.Fatal("dangling alias created a separate lock domain")
	}
}

func TestOwnershipReleasedByKernelAfterProcessDeath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOwnershipHolder$")
	command.Env = append(os.Environ(), "GSEA_OWNERSHIP_HOLDER_PATH="+path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("holder startup: %q err=%v", line, err)
	}
	if owner, err := Acquire(path); !errors.Is(err, ErrOwned) {
		if owner != nil {
			_ = owner.Close()
		}
		t.Fatalf("holder ownership not enforced: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	owner, err := Acquire(path)
	if err != nil {
		t.Fatalf("dead process retained lock: %v", err)
	}
	defer owner.Close()
}

func TestOwnershipHolder(t *testing.T) {
	path := os.Getenv("GSEA_OWNERSHIP_HOLDER_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	owner, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
