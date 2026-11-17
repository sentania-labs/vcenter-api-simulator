// Package storage identifies exhausted persistent storage without hiding other IO errors.
package storage

import (
	"errors"
	"syscall"
)

func Full(err error) bool { return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) }
