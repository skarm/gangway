package gangway

import "strings"

const (
	containerIDLength = 64
	// Docker rejects longer references well before this, but a hard cap keeps
	// an oversized target out of the upstream request either way.
	maxImageReferenceBytes = 1024
	minAPIVersionLength    = 3
	maxAPIVersionLength    = 16
)

// trimAPIVersion removes a leading "/vX.Y" segment and returns the rest of the
// path. Inspect endpoints must be versioned, so a missing or malformed version
// is a rejection rather than a fallback to the unversioned form.
func trimAPIVersion(path string) (string, bool) {
	const prefix = "/v"

	if !strings.HasPrefix(path, prefix) {
		return "", false
	}

	end := strings.IndexByte(path[len(prefix):], '/')
	if end < 0 {
		return "", false
	}

	end += len(prefix)

	if !validAPIVersion(path[len(prefix):end]) {
		return "", false
	}

	return path[end:], true
}

// validAPIVersion accepts the "major.minor" form Docker clients send, such as
// the "1.55" in /v1.55/containers/<id>/json.
func validAPIVersion(version string) bool {
	if len(version) < minAPIVersionLength || len(version) > maxAPIVersionLength {
		return false
	}

	dot := strings.IndexByte(version, '.')
	if dot <= 0 || dot == len(version)-1 || strings.IndexByte(version[dot+1:], '.') >= 0 {
		return false
	}

	for i := 0; i < len(version); i++ {
		if i == dot {
			continue
		}

		if !isDigit(version[i]) {
			return false
		}
	}

	return true
}

// validContainerID accepts only a full-length lowercase hexadecimal ID. Short
// IDs and name lookups are not forwarded: SPIRE resolves the full ID itself,
// and a name would let a caller reach containers by guessing.
func validContainerID(id string) bool {
	if len(id) != containerIDLength {
		return false
	}

	for i := 0; i < len(id); i++ {
		if !isHexDigit(id[i]) {
			return false
		}
	}

	return true
}

// validImageReference accepts the character set Docker uses for image
// references and rejects empty, "." and ".." path segments, which also covers
// leading, trailing and repeated separators.
func validImageReference(ref string) bool {
	if len(ref) == 0 || len(ref) > maxImageReferenceBytes {
		return false
	}

	start := 0

	for i := 0; i <= len(ref); i++ {
		if i < len(ref) && ref[i] != '/' {
			if !isImageReferenceByte(ref[i]) {
				return false
			}

			continue
		}

		switch ref[start:i] {
		case "", ".", "..":
			return false
		}

		start = i + 1
	}

	return true
}

func isImageReferenceByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', isDigit(c):
		return true
	case c == '.', c == '_', c == ':', c == '@', c == '+', c == '-':
		return true
	default:
		return false
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'f') }
