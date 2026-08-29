package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/crashcartapp/crashcart/internal/symbolicate"
)

// maxJSONBody bounds JSON request bodies.
const maxJSONBody = 1 << 20

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// writeErr writes {"error": msg} with the given status.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// errNotFound is returned by lookups that should surface as 404.
var errNotFound = errors.New("not found")

// fail maps an error to a response: ErrNoRows / errNotFound → 404, a
// badRequest → 400, anything else → 500 (logged).
func (h *Handler) fail(w http.ResponseWriter, err error) {
	var br badRequest
	var ue symbolicate.UploadError
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.As(err, &br):
		writeErr(w, http.StatusBadRequest, br.Error())
	case errors.As(err, &ue):
		writeErr(w, http.StatusBadRequest, ue.Error())
	default:
		if h.Log != nil {
			h.Log.Error("api", "err", err)
		}
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// badRequest is a client error carried through fail.
type badRequest string

func (b badRequest) Error() string { return string(b) }

// readJSON decodes a bounded JSON body into v.
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		return badRequest("body too large or unreadable")
	}
	if len(body) == 0 {
		return badRequest("empty body")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return badRequest("invalid JSON: " + err.Error())
	}
	return nil
}
