//go:build !linux

package gangway

import "net"

// supportPeerCredentials refuses a peer policy on a platform with no
// SO_PEERCRED, rather than letting one be configured and silently ignored. The
// proxy targets Linux; this file exists so the package still builds and tests
// elsewhere.
func supportPeerCredentials() error { return ErrPeerCredentialsUnsupported }

func peerCredentialsOf(_ net.Conn) (peerCredentials, error) {
	return peerCredentials{}, ErrPeerCredentialsUnsupported
}
