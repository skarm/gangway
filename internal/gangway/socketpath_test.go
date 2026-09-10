package gangway_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/skarm/gangway/internal/gangway"
)

func TestValidatePathsRejectsAliasesOfTheDockerSocket(t *testing.T) {
	dir := socketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	dockerPath := filepath.Join(dir, "docker.sock")

	aliasDir := filepath.Join(socketDir(t), "alias")
	if err := os.Symlink(dir, aliasDir); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.sock")
	if err := os.Symlink(listenPath, dangling); err != nil {
		t.Fatal(err)
	}

	for name, paths := range map[string][2]string{
		"identical":         {listenPath, listenPath},
		"unclean":           {dir + "/nested/../proxy.sock", listenPath},
		"through symlink":   {filepath.Join(aliasDir, "proxy.sock"), listenPath},
		"dangling symlink":  {listenPath, dangling},
		"relative listen":   {"relative.sock", dockerPath},
		"relative upstream": {listenPath, "relative.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := gangway.ValidatePaths(paths[0], paths[1]); err == nil {
				t.Errorf("unsafe paths accepted: %q and %q", paths[0], paths[1])
			}
		})
	}

	if err := gangway.ValidatePaths(listenPath, dockerPath); err != nil {
		t.Fatalf("different not-yet-created sockets rejected: %v", err)
	}
	if err := os.WriteFile(dockerPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gangway.ValidatePaths(listenPath, dockerPath); err == nil {
		t.Fatal("regular upstream file accepted")
	}
}

// TestValidatePathsDetectsHardlink covers the case distinct names cannot catch:
// two paths that already share one inode.
func TestValidatePathsDetectsHardlink(t *testing.T) {
	dir := socketDir(t)
	dockerPath, alias := filepath.Join(dir, "docker.sock"), filepath.Join(dir, "proxy.sock")
	listener, err := net.Listen("unix", dockerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Link(dockerPath, alias); err != nil {
		t.Fatal(err)
	}
	if err := gangway.ValidatePaths(alias, dockerPath); err == nil {
		t.Fatal("hardlinked Docker socket accepted as listen socket")
	}
}
