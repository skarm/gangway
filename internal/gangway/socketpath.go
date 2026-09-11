package gangway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// resolvePath expands existing symlinks in an absolute path, including in the
// parents of a socket that does not exist yet. filepath.EvalSymlinks cannot be
// used here because it requires the whole path to exist.
//
// Components are applied from the root outwards and ".." is resolved against
// the prefix already expanded, which is the order the kernel reads a path in.
// Cleaning the path up front instead would remove ".." lexically and rewrite
// "dir/link/.." into "dir", naming a directory the path never referred to: a
// listen socket spelled that way could still land on the Docker socket.
func resolvePath(path string) (string, error) {
	root := string(filepath.Separator)
	resolved := root
	pending := pathComponents(path)

	// One budget for depth and symlink hops together, since expanding a link
	// puts its target's components back in the queue.
	for budget := maxPathComponents; len(pending) > 0; budget-- {
		if budget == 0 {
			return "", errors.New("too many symlinks or path components in socket path")
		}

		name, rest := pending[0], pending[1:]
		pending = rest

		switch name {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}

		next := filepath.Join(resolved, name)

		info, err := os.Lstat(next)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// There is nothing to expand below a component that does not exist
			// yet, but the components after it still apply to it.
			resolved = next
		case err != nil:
			return "", fmt.Errorf("resolve socket path %q: %w", next, err)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(next)
			if err != nil {
				return "", fmt.Errorf("resolve socket path %q: %w", next, err)
			}
			// A relative target is read from the directory holding the link,
			// which is what resolved already names.
			if filepath.IsAbs(target) {
				resolved = root
			}

			pending = append(pathComponents(target), rest...)
		default:
			resolved = next
		}
	}

	return resolved, nil
}

// pathComponents splits path into the names resolvePath applies one at a time,
// dropping the empty strings that leading, trailing and repeated separators
// produce.
func pathComponents(path string) []string {
	parts := strings.Split(path, string(filepath.Separator))
	components := make([]string, 0, len(parts))

	for _, part := range parts {
		if part != "" {
			components = append(components, part)
		}
	}

	return components
}
