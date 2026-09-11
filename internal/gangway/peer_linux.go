package gangway

import (
	"fmt"
	"net"
	"syscall"
)

// supportPeerCredentials reports whether a peer policy can be enforced on this
// platform. Linux has SO_PEERCRED, so it can.
func supportPeerCredentials() error { return nil }

// peerCredentialsOf asks the kernel which process opened the other end of conn.
// SO_PEERCRED is recorded at connect time and cannot be set by the peer, so it
// identifies the client even when the socket permissions would admit more than
// one. The IDs are those of the listener's user namespace.
func peerCredentialsOf(conn net.Conn) (peerCredentials, error) {
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		return peerCredentials{}, fmt.Errorf("connection of type %T exposes no file descriptor", conn)
	}

	raw, err := syscallConn.SyscallConn()
	if err != nil {
		return peerCredentials{}, fmt.Errorf("access connection: %w", err)
	}

	var (
		ucred     *syscall.Ucred
		sockErr   error
		controlOK bool
	)

	err = raw.Control(func(fd uintptr) {
		controlOK = true
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return peerCredentials{}, fmt.Errorf("access connection: %w", err)
	}

	if !controlOK {
		return peerCredentials{}, fmt.Errorf("connection closed before its peer could be identified")
	}

	if sockErr != nil {
		return peerCredentials{}, fmt.Errorf("read peer credentials: %w", sockErr)
	}

	return peerCredentials{pid: ucred.Pid, uid: ucred.Uid, gid: ucred.Gid}, nil
}
