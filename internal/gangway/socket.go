package gangway

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// lockSuffix names the file whose advisory lock marks a live instance. It
	// is deliberately left on disk after shutdown: removing it would let two
	// processes lock different inodes under the same name during a restart.
	lockSuffix = ".lock"
	// stagingPrefix names the private directory a socket is bound in before it
	// is published under its final name.
	stagingPrefix = ".gangway-"
	// socketDirMode keeps the socket directory traversable by its group, which
	// the 0660 socket mode relies on, and writable by nobody else.
	socketDirMode = 0o750
	// staleDialTimeout bounds the probe deciding whether a leftover socket
	// still has a listener behind it.
	staleDialTimeout = 250 * time.Millisecond
)

// unixListener removes its socket and releases its lock on Close.
type unixListener struct {
	*net.UnixListener
	path      string
	info      os.FileInfo
	lock      *os.File
	closeOnce sync.Once
	closeErr  error
}

func (l *unixListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.UnixListener.Close()
		// A socket published by a later instance belongs to that instance.
		// Only unlink the pathname while it still names our own inode.
		if info, err := os.Lstat(l.path); err == nil && os.SameFile(info, l.info) {
			l.closeErr = errors.Join(l.closeErr, os.Remove(l.path))
		}

		l.closeErr = errors.Join(l.closeErr, l.lock.Close())
	})

	return l.closeErr
}

// Listen publishes a Unix socket at path with the given mode, refusing to run
// alongside another instance or to disturb a file it did not create. mode must
// be 0600 or 0660; anything wider would hand out the Docker API more freely
// than the proxy's own access to it.
func Listen(path string, mode os.FileMode) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("listen socket path must be absolute")
	}
	// Everything below reads the socket's directory and lock name off this
	// path with filepath.Dir and a suffix, which resolve ".." lexically while
	// the kernel resolves it after the symlink before it. Requiring a clean
	// path keeps the two from naming different directories, rather than
	// checking one and binding in the other.
	if filepath.Clean(path) != path {
		return nil, fmt.Errorf(`listen socket path must be clean, without "..", "." or repeated separators: %q`, path)
	}

	if mode != 0o600 && mode != 0o660 {
		return nil, errors.New("socket permissions must be 0600 or 0660")
	}

	dir := filepath.Dir(path)

	if err := createSocketDir(dir); err != nil {
		return nil, err
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect socket directory: %w", err)
	}

	if !privateToEffectiveUser(dirInfo) {
		return nil, errors.New("socket directory must be owned by the current user and not writable by group or others")
	}
	// A socket the group may use is only reachable if the group may also
	// traverse the directory holding it. Serving on one it cannot is a running
	// proxy no client can reach, which is worse than refusing to start. Only
	// this directory is checked: it is the one that travels with the socket
	// into the client's mount namespace, while its parents are the operator's
	// to arrange and may not even be the parents the client sees.
	if mode&0o060 != 0 && dirInfo.Mode().Perm()&0o010 == 0 {
		return nil, fmt.Errorf("socket mode %04o needs a group-traversable socket directory, but %q is mode %04o", mode, dir, dirInfo.Mode().Perm())
	}

	lock, err := acquireInstanceLock(path + lockSuffix)
	if err != nil {
		return nil, err
	}

	keepLock := false
	defer func() {
		if !keepLock {
			_ = lock.Close()
		}
	}()

	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}

	listener, info, err := bindAndPublish(dir, path, mode)
	if err != nil {
		return nil, err
	}

	keepLock = true

	return &unixListener{UnixListener: listener, path: path, info: info, lock: lock}, nil
}

// createSocketDir creates dir, and any missing parent of it, with
// socketDirMode. The mode is applied after the fact because Mkdir takes the
// process umask into account: under a umask of 077 the directory would come
// out 0700, and a 0660 socket inside it would be unreachable for the group the
// mode was widened for. A directory that already exists is left exactly as the
// operator prepared it, and checked by the caller instead.
func createSocketDir(dir string) error {
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("socket directory is not a directory: %q", dir)
		}

		return nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect socket directory: %w", err)
	}

	if parent := filepath.Dir(dir); parent != dir {
		if err := createSocketDir(parent); err != nil {
			return err
		}
	}

	if err := os.Mkdir(dir, socketDirMode); err != nil {
		// Someone else created it between the Stat above and here, so it is
		// theirs to have set up; the caller still has to approve it.
		if errors.Is(err, os.ErrExist) {
			return nil
		}

		return fmt.Errorf("create socket directory: %w", err)
	}

	if err := os.Chmod(dir, socketDirMode); err != nil {
		return fmt.Errorf("set socket directory permissions: %w", err)
	}

	return nil
}

// acquireInstanceLock opens and exclusively locks the instance lock file. The
// lock, not the presence of the socket, is what makes a second instance fail.
func acquireInstanceLock(path string) (*os.File, error) {
	// O_NOFOLLOW prevents a planted symlink from redirecting this operation.
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open socket lock: %w", err)
	}

	lock := os.NewFile(uintptr(fd), path)

	info, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("inspect socket lock: %w", err)
	}

	if !info.Mode().IsRegular() || !privateToEffectiveUser(info) {
		_ = lock.Close()
		return nil, errors.New("socket lock must be a regular file owned by the current user and not writable by group or others")
	}

	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("lock socket (another proxy may be running): %w", err)
	}

	return lock, nil
}

// bindAndPublish binds the socket inside a private directory and publishes it
// by rename once its mode is set, so the process umask never widens the socket
// during the window between bind and chmod.
func bindAndPublish(dir, path string, mode os.FileMode) (*net.UnixListener, os.FileInfo, error) {
	staging, err := os.MkdirTemp(dir, stagingPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("create private socket directory: %w", err)
	}
	defer os.RemoveAll(staging)

	staged := filepath.Join(staging, "s")

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: staged, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	// The listener must not unlink the published name on Close; unixListener
	// decides that itself after checking the inode still belongs to it.
	listener.SetUnlinkOnClose(false)

	info, err := os.Lstat(staged)
	if err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("inspect created socket: %w", err)
	}

	if err := os.Chmod(staged, mode); err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("set socket permissions: %w", err)
	}
	// Only this user can write the directory and only one instance holds the
	// lock, but still refuse to replace an entry created by another program.
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		_ = listener.Close()
		return nil, nil, errors.New("listen path appeared while creating the socket")
	}

	if err := os.Rename(staged, path); err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("publish socket: %w", err)
	}

	return listener, info, nil
}

// removeStaleSocket clears a socket left behind by a crashed instance, but only
// after proving nothing listens on it and that it did not change in between.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("inspect socket path: %w", err)
	}

	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %q", path)
	}

	conn, err := net.DialTimeout("unix", path, staleDialTimeout)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("socket is already in use: %q", path)
	}
	// Only a refused connection proves the socket has no listener. Any other
	// failure, a timeout included, leaves the socket untouched.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot prove existing socket is stale: %w", err)
	}

	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck stale socket: %w", err)
	}

	if !os.SameFile(info, current) {
		return errors.New("socket changed while checking whether it was stale")
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	return nil
}

// privateToEffectiveUser reports whether only this process's effective user can
// modify the file. The type assertion is checked so an unexpected FileInfo
// fails closed instead of panicking inside a security check.
func privateToEffectiveUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().Perm()&0o022 == 0 && stat.Uid == uint32(os.Geteuid())
}
