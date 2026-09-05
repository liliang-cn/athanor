package server

import (
	"io"
	"strings"
)

func stringsReader(s string) io.Reader {
	if s == "" {
		return nil
	}
	return strings.NewReader(s)
}
