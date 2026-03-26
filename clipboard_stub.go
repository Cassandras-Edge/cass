//go:build !darwin

package main

import (
	"io"

	"golang.org/x/crypto/ssh"
)

type stubBridge struct{}

func newClipboardBridge(_ *ssh.Client) Bridge {
	return &stubBridge{}
}

func (b *stubBridge) WrapStdin(stdin io.Reader) io.Reader {
	return stdin
}
