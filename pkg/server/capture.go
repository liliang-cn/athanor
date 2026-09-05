package server

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// captureWriter lets the front page reuse the loads handler without a second
// implementation of the load.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captureWriter) WriteHeader(code int)        { c.status = code }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
