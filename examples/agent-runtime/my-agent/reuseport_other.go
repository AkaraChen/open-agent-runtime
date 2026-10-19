//go:build !linux

package main

import "syscall"

func setReusePort(network, address string, c syscall.RawConn) error { return nil }
