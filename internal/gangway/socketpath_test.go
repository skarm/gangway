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

// TestValidatePathsResolvesDotDotAfterSymlinks covers a listen path whose ".."
// only names what the kernel says it names once the symlink before it has been
// expanded: with A/link pointing at B/nested, "A/link/../docker.sock" is
// B/docker.sock rather than A/docker.sock. Resolving it the other way round
// let the proxy publish its socket at the Docker socket's own address, as long
// as the daemon had not created that socket yet.
func TestValidatePathsResolvesDotDotAfterSymlinks(t *testing.T) {
	base := socketDir(t)
	nested := filepath.Join(base, "B", "nested")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "A"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "A", "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}

	// Spelled out rather than joined, because filepath.Join would clean the
	// ".." away before ValidatePaths ever saw it.
	listenPath := link + "/../docker.sock"

	if err := gangway.ValidatePaths(listenPath, filepath.Join(base, "B", "docker.sock")); err == nil {
		t.Error("listen path that resolves onto the Docker socket accepted")
	}
	// The same spelling is fine on the Docker socket, which is only ever
	// dialed: a ".." in a path is not itself the problem, and Listen is what
	// requires the listen path to be clean.
	if err := gangway.ValidatePaths(filepath.Join(base, "A", "docker.sock"), listenPath); err != nil {
		t.Errorf("distinct sockets rejected: %v", err)
	}
}

// TestResolvePathAgreesWithEvalSymlinks holds the resolver against the standard
// library's for every path that exists in full, which is the only case
// filepath.EvalSymlinks handles and the only one with an independent answer.
// Sockets that do not exist yet are what the resolver is written for, but a
// resolver that disagrees with the kernel on the paths that do exist would not
// be right about those either.
func TestResolvePathAgreesWithEvalSymlinks(t *testing.T) {
	// The temporary directory is resolved first: /tmp is itself a symlink on
	// macOS, and every expected value below would carry it otherwise.
	base, err := filepath.EvalSymlinks(socketDir(t))
	if err != nil {
		t.Fatal(err)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(base, "a", "b", "c"), 0o750))
	must(os.MkdirAll(filepath.Join(base, "x", "y"), 0o750))
	must(os.WriteFile(filepath.Join(base, "a", "b", "c", "file"), []byte("f"), 0o600))
	must(os.Symlink(filepath.Join(base, "a", "b"), filepath.Join(base, "abs")))
	must(os.Symlink("../x/y", filepath.Join(base, "a", "rel")))
	must(os.Symlink("b/c", filepath.Join(base, "a", "deep")))
	must(os.Symlink(filepath.Join(base, "abs"), filepath.Join(base, "chain")))
	must(os.Symlink("chain", filepath.Join(base, "chain2")))
	must(os.Symlink(filepath.Join(base, "a", "rel"), filepath.Join(base, "x", "back")))

	for _, path := range []string{
		base,
		base + "/a/b/c",
		base + "/a/b/c/file",
		base + "/abs",
		base + "/abs/c/file",
		base + "/abs/..",
		base + "/abs/../b/c",
		base + "/a/rel",
		base + "/a/rel/..",
		base + "/a/deep/file",
		base + "/chain/c",
		base + "/chain2/c/file",
		base + "/x/back/../y",
		base + "/a/./b/././c",
		base + "//a///b//c",
		base + "/a/b/c/../../b/c/file",
		base + "/../" + filepath.Base(base) + "/abs/c",
	} {
		want, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("no independent answer for %q: %v", path, err)
		}
		got, err := gangway.ResolvePath(path)
		if err != nil {
			t.Errorf("ResolvePath(%q) failed: %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("ResolvePath(%q) = %q, EvalSymlinks = %q", path, got, want)
		}
	}
}

// TestResolvePathRefusesSymlinkLoops keeps a cycle from becoming an unbounded
// walk: the budget covers symlink hops and path depth together, because
// expanding a link puts its target's components back in the queue.
func TestResolvePathRefusesSymlinkLoops(t *testing.T) {
	base := socketDir(t)
	if err := os.Symlink(filepath.Join(base, "b"), filepath.Join(base, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "a"), filepath.Join(base, "b")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("self", filepath.Join(base, "self")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{base + "/a", base + "/b", base + "/a/deeper", base + "/self"} {
		if got, err := gangway.ResolvePath(path); err == nil {
			t.Errorf("ResolvePath(%q) = %q, want a refusal", path, got)
		}
	}
}
