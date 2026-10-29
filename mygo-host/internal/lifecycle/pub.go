package lifecycle

import (
	"encoding/json"
	"io"
	"net/http"
)

// Publication is one <root>/server-list line (exported view).
type Publication struct {
	PID  int
	Port int
}

// PIDStatus mirrors daemon/pid-status.
type PIDStatus int

const (
	PIDAlive PIDStatus = iota
	PIDNotFound
	PIDNoPermission
	PIDUnknown
)

// PIDStatusOf classifies a pid.
func PIDStatusOf(pid int) PIDStatus {
	err := pidExistsErr(pid)
	switch {
	case err == nil:
		return PIDAlive
	case isESRCH(err):
		return PIDNotFound
	case isEPERM(err):
		return PIDNoPermission
	default:
		return PIDUnknown
	}
}

// Entries exports the server-list reader.
func Entries(root string) ([]Publication, error) {
	pubs, err := entries(root)
	if err != nil {
		return nil, err
	}
	out := make([]Publication, 0, len(pubs))
	for _, p := range pubs {
		out = append(out, Publication{PID: p.pid, Port: p.port})
	}
	return out, nil
}

// RemoveEntries exports the server-list rewrite under its lock.
func RemoveEntries(root string, removed []Publication) error {
	pubs := make([]*target, 0, len(removed))
	for _, p := range removed {
		// removeEntries only consumes pid+port from each target
		pubs = append(pubs, &target{PID: p.PID, Port: p.Port})
	}
	return removeEntries(root, pubs)
}

// ParseJSONBody parses a JSON object body into a generic map.
func ParseJSONBody(body []byte) (map[string]any, error) {
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// ReadBody drains an http response body.
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ErrorParts extracts code/message from a lifecycle error.
func ErrorParts(err error) (code, message string) {
	var le *Error
	if asLifecycleError(err, &le) {
		return le.Code, le.Message
	}
	if err == nil {
		return "", ""
	}
	return "server-stop-failed", err.Error()
}
