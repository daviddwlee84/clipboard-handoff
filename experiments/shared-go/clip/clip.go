// Package clip wraps golang.design/x/clipboard behind a lazy initializer so the
// daemon starts (and the text/file round-trip works) even where no OS clipboard
// is available — clipboard access is only required for `paste` and
// `auto_copy on`. PNG is the native image format on both the wire and this
// clipboard, so image bytes pass through byte-for-byte.
package clip

import (
	"errors"
	"sync"

	"golang.design/x/clipboard"
)

var (
	once    sync.Once
	initErr error
)

// ensure initializes the clipboard exactly once. clipboard.Init may fail (or on
// some platforms panic) when no display/pasteboard is reachable; we recover and
// surface it as an error so callers degrade gracefully.
func ensure() (err error) {
	once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				initErr = errors.New("clipboard unavailable on this host")
			}
		}()
		initErr = clipboard.Init()
	})
	return initErr
}

// WriteText places text on the OS clipboard.
func WriteText(s string) error {
	if err := ensure(); err != nil {
		return err
	}
	clipboard.Write(clipboard.FmtText, []byte(s))
	return nil
}

// WritePNG places PNG image bytes on the OS clipboard (golang.design's image
// format is PNG).
func WritePNG(png []byte) error {
	if err := ensure(); err != nil {
		return err
	}
	clipboard.Write(clipboard.FmtImage, png)
	return nil
}
