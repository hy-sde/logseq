package host

import (
	"logseq/mygo-host/internal/transit"
)

// LqSync handles the ":electron.server/sync!<syncId>" channels.
//
// Round trip (listener.cljs:110-135): the daemon pushes an
// "invokeLogseqAPI" event {syncId, method, args} to the renderer; the
// renderer invokes the method locally and replies over
// window.apis.invoke(":electron.server/sync!<syncId>", [result]) or
// [{error: message}]. The Go side only needs to accept the reply — the
// original request was delivered by the daemon directly.
type LqSync struct{ H *Host }

// Handle accepts [result] / [{error: message}] replies. There is nothing
// to resolve locally in milestone 1 (sync client state lives with the
// daemon); we acknowledge and drop.
func (s *LqSync) Handle(channel string, args []any) (any, error) {
	_ = channel
	_ = args
	return nil, nil
}

// decodeTransitArgs is a helper for channels that receive transit strings.
func decodeTransitArgs(args []any) ([]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	if s, ok := args[0].(string); ok {
		decoded, err := transit.DecodeArray(s)
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	return args, nil
}
