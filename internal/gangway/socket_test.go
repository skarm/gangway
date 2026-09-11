package gangway_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/skarm/gangway/internal/gangway"
)

func TestListenRefusesToDisturbAnActiveSocket(t *testing.T) {
	path := filepath.Join(socketDir(t), "active.sock")
	active, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = active.Close() })
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if listener, err := gangway.Listen(path, 0o600); err == nil {
		_ = listener.Close()
		t.Fatal("replaced an active socket")
	}

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("active socket changed: %v", err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("original listener is no longer reachable: %v", err)
	}
	_ = conn.Close()
}

// TestListenLockOutlivesTheSocketName checks that the instance lock, not the
// presence of the socket file, is what keeps a second instance out.
func TestListenLockOutlivesTheSocketName(t *testing.T) {
	path := filepath.Join(socketDir(t), "proxy.sock")
	first, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if second, err := gangway.Listen(path, 0o600); err == nil {
		_ = second.Close()
		t.Fatal("second listener acquired an already held lock")
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatalf("restart after lock release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed on close: %v", err)
	}
}

func TestListenRecoversAStaleSocket(t *testing.T) {
	path := filepath.Join(socketDir(t), "stale.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatalf("recover stale socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("recovered listener is not reachable: %v", err)
	}
	_ = conn.Close()
}

// TestCloseKeepsAReplacementSocket guards the restart case: a listener must
// never unlink a socket a newer instance published under the same name.
func TestCloseKeepsAReplacementSocket(t *testing.T) {
	path := filepath.Join(socketDir(t), "proxy.sock")
	first, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	want, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.Lstat(path)
	if err != nil || !os.SameFile(want, got) {
		t.Fatalf("closing the old listener removed the replacement: %v", err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("replacement unreachable: %v", err)
	}
	_ = conn.Close()
}

func TestListenRejectsUnsafeFilesAndPermissions(t *testing.T) {
	for _, kind := range []string{"regular file", "symlink", "lock symlink", "writable directory", "invalid mode", "unclean path"} {
		t.Run(kind, func(t *testing.T) {
			dir := socketDir(t)
			path := filepath.Join(dir, "proxy.sock")
			mode := os.FileMode(0o600)
			protected := filepath.Join(dir, "keep")
			if err := os.WriteFile(protected, []byte("preserve me"), 0o600); err != nil {
				t.Fatal(err)
			}

			switch kind {
			case "regular file":
				if err := os.WriteFile(path, []byte("preserve me"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(protected, path); err != nil {
					t.Fatal(err)
				}
			case "lock symlink":
				if err := os.Symlink(protected, path+".lock"); err != nil {
					t.Fatal(err)
				}
			case "writable directory":
				if err := os.Chmod(dir, 0o770); err != nil {
					t.Fatal(err)
				}
			case "invalid mode":
				mode = 0o666
			case "unclean path":
				// filepath.Dir would read this as dir, while the kernel reads
				// it as whatever "sub" turns out to be the parent of.
				path = dir + "/sub/../proxy.sock"
			}

			if listener, err := gangway.Listen(path, mode); err == nil {
				_ = listener.Close()
				t.Fatal("unsafe listen configuration accepted")
			}
			if data, err := os.ReadFile(protected); err != nil || string(data) != "preserve me" {
				t.Fatalf("protected file modified: %q, %v", data, err)
			}
			if kind == "regular file" || kind == "symlink" {
				if data, err := os.ReadFile(path); err != nil || string(data) != "preserve me" {
					t.Fatalf("existing path modified: %q, %v", data, err)
				}
			}
		})
	}
}

func TestListenAppliesSocketPermissions(t *testing.T) {
	// A umask that strips every group bit off anything created under it, which
	// is what makes the directory mode worth asserting instead of assuming.
	previous := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(previous) })

	for _, mode := range []os.FileMode{0o600, 0o660} {
		dir := filepath.Join(socketDir(t), "run")
		path := filepath.Join(dir, "proxy.sock")
		listener, err := gangway.Listen(path, mode)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })

		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode || info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("socket mode = %v, want socket with %v", info.Mode(), mode)
		}
		// The umask must not reach a directory the proxy created for itself:
		// a 0700 one would leave the 0660 socket above unreachable.
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0o750 {
			t.Fatalf("socket directory mode = %04o, want 0750", dirInfo.Mode().Perm())
		}
	}
}

// TestListenRejectsAGroupSocketInAPrivateDirectory keeps the proxy from
// serving where nothing can reach it. A directory the operator prepared as
// 0700 contradicts a socket mode that invites the group in, and a proxy that
// starts anyway looks healthy while every client gets EACCES on the directory.
func TestListenRejectsAGroupSocketInAPrivateDirectory(t *testing.T) {
	dir := socketDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "proxy.sock")

	if listener, err := gangway.Listen(path, 0o660); err == nil {
		_ = listener.Close()
		t.Fatal("0660 socket accepted in a directory its group cannot traverse")
	}
	// Nothing may be left behind for the next attempt, including the lock.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected configuration left %d entries behind", len(entries))
	}
	// The same directory is right for a socket only its owner may use.
	listener, err := gangway.Listen(path, 0o600)
	if err != nil {
		t.Fatalf("0600 socket rejected: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}
