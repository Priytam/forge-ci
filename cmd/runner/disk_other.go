//go:build !unix

package main

import "errors"

func diskFree(string) (avail, total uint64, err error) {
	return 0, 0, errors.New("free space is not read on this platform")
}
