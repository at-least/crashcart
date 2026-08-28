package ingest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
)

func newRequest(method, path string, body []byte) *http.Request {
	return httptest.NewRequest(method, path, bytes.NewReader(body))
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
