//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"golang.design/x/clipboard"
	"golang.org/x/crypto/ssh"
)

var (
	bracketedPasteStart = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

type darwinBridge struct {
	client *ssh.Client
}

func newClipboardBridge(client *ssh.Client) Bridge {
	// Initialize clipboard (required by golang.design/x/clipboard on macOS)
	if err := clipboard.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard init failed (images won't work): %v\n", err)
	}
	return &darwinBridge{client: client}
}

func (b *darwinBridge) WrapStdin(stdin io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go b.intercept(stdin, pw)
	return pr
}

func (b *darwinBridge) intercept(src io.Reader, dst *io.PipeWriter) {
	defer dst.Close()

	buf := make([]byte, 32*1024)
	var pending []byte

	for {
		n, err := src.Read(buf)
		if n > 0 {
			data := buf[:n]

			// Accumulate data to detect bracketed paste sequences
			pending = append(pending, data...)

			for {
				startIdx := bytes.Index(pending, bracketedPasteStart)
				if startIdx == -1 {
					// No paste start found — flush everything
					if _, werr := dst.Write(pending); werr != nil {
						return
					}
					pending = nil
					break
				}

				// Flush everything before the paste start
				if startIdx > 0 {
					if _, werr := dst.Write(pending[:startIdx]); werr != nil {
						return
					}
					pending = pending[startIdx:]
				}

				// Look for paste end
				endIdx := bytes.Index(pending, bracketedPasteEnd)
				if endIdx == -1 {
					// Paste end not yet received — wait for more data
					break
				}

				// Full paste sequence captured — sync clipboard image
				b.syncImage()

				// Pass the entire paste sequence through unmodified
				pasteEnd := endIdx + len(bracketedPasteEnd)
				if _, werr := dst.Write(pending[:pasteEnd]); werr != nil {
					return
				}
				pending = pending[pasteEnd:]
			}
		}
		if err != nil {
			if err != io.EOF {
				dst.CloseWithError(err)
			}
			// Flush remaining
			if len(pending) > 0 {
				_, _ = dst.Write(pending)
			}
			return
		}
	}
}

func (b *darwinBridge) syncImage() {
	imgData := clipboard.Read(clipboard.FmtImage)
	if len(imgData) == 0 {
		return
	}

	// Open a separate SSH session to write to xclip
	session, err := b.client.NewSession()
	if err != nil {
		return
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(imgData)
	_ = session.Run("xclip -selection clipboard -t image/png -i")
}
