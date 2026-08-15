package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

type sameUIDListener struct {
	*net.UnixListener
	uid uint32
}

func (l *sameUIDListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := unixPeerUID(connection)
		if err == nil && uid == l.uid {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func unixPeerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credential *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credential, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credentialErr != nil {
		return 0, credentialErr
	}
	return credential.Uid, nil
}
