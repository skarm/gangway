package gangway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxPathComponents budgets both symlink hops and path depth while resolving.
const maxPathComponents = 255

// ValidatePaths rejects configurations where the listen socket and the Docker
// socket are, or could become, the same file. Serving on the Docker socket
// would replace the daemon's own endpoint.
func ValidatePaths(listenPath, dockerPath string) error {
	for _, path := range []string{listenPath, dockerPath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("socket path must be absolute: %q", path)
		}
	}

	listenResolved, err := resolvePath(listenPath)
	if err != nil {
		return err
	}

	dockerResolved, err := resolvePath(dockerPath)
	if err != nil {
		return err
	}

	if listenResolved == dockerResolved {
		return errors.New("listen socket and Docker socket must be different paths")
	}

	listenInfo, listenErr := os.Stat(listenPath)
	dockerInfo, dockerErr := os.Stat(dockerPath)
	if dockerErr != nil && !errors.Is(dockerErr, os.ErrNotExist) {
		return fmt.Errorf("inspect Docker socket: %w", dockerErr)
	}

	if dockerErr == nil {
		if dockerInfo.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("docker socket is not a Unix socket: %q", dockerPath)
		}
		// Distinct names can still be one inode; a hard link would otherwise
		// pass the comparison above.
		if listenErr == nil && os.SameFile(listenInfo, dockerInfo) {
			return errors.New("listen socket and Docker socket refer to the same file")
		}
	}

	return nil
}

// resolvePath expands existing symlinks in path, including in the parents of a
// socket that does not exist yet. filepath.EvalSymlinks cannot be used here
// because it requires the whole path to exist.
func resolvePath(path string) (string, error) {
	return resolvePathWithin(path, maxPathComponents)
}

func resolvePathWithin(path string, remaining int) (string, error) {
	if remaining == 0 {
		return "", errors.New("too many symlinks or path components in socket path")
	}

	path = filepath.Clean(path)

	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}

		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}

		return resolvePathWithin(target, remaining-1)
	}

	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve socket path %q: %w", path, err)
	}

	parent := filepath.Dir(path)
	if parent == path {
		return path, err
	}

	resolvedParent, err := resolvePathWithin(parent, remaining-1)
	if err != nil {
		return "", err
	}

	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
