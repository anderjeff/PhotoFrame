//go:build !linux

package frame

import "errors"

// OpenFramebuffer is only available on Linux; elsewhere the frame runs headless.
func OpenFramebuffer(dev, tty string) (Display, error) {
	return nil, errors.New("framebuffer output is only supported on linux")
}
