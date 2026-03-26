package main

import (
	"io"

	"golang.org/x/crypto/ssh"
)

// ClipboardBridge intercepts paste events in stdin and syncs clipboard images
// to the remote pod's xclip. The actual implementation is platform-specific:
// clipboard_darwin.go (macOS) and clipboard_stub.go (everything else).

// Bridge is the interface both implementations satisfy.
type Bridge interface {
	// WrapStdin returns a reader that intercepts bracketed paste sequences.
	// On macOS, when a paste is detected, it checks the local clipboard for
	// image data and writes it to the remote pod's xclip before passing the
	// paste through.
	WrapStdin(stdin io.Reader) io.Reader
}

// NewClipboardBridge creates a platform-specific clipboard bridge.
// The SSH client is used to open separate channels for xclip writes.
func NewClipboardBridge(client *ssh.Client) Bridge {
	return newClipboardBridge(client)
}
