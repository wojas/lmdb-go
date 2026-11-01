//go:build !windows
// +build !windows

package lmdb

import "syscall"

func operrno(op string, ret int) error {
	if ret == success {
		return nil
	}
	if minErrno <= int32(ret) && int32(ret) <= maxErrno {
		return &OpError{Op: op, Errno: Errno(ret)}
	}
	return &OpError{Op: op, Errno: syscall.Errno(ret)}
}

// errNotOwner is what unlocking another thread's writer mutex returns
// (pthread_mutex_unlock of a robust mutex, Linux and FreeBSD; semaphores,
// which belong to no thread, never fail so).
const errNotOwner = syscall.EPERM
