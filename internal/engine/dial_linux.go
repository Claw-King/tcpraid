package engine

import "syscall"

func markControl(mark int) func(_, _ string, rc syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
		}); err != nil {
			return err
		}
		return serr
	}
}
