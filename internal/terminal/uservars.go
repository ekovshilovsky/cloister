package terminal

import (
	"encoding/base64"
	"fmt"
)

// setUserVar emits OSC 1337 SetUserVar=KEY=BASE64(VALUE), the iTerm2
// proprietary mechanism for stamping per-session user variables that can be
// referenced from a tab- or window-title format string as "\(user.KEY)".
//
// The variable persists for the lifetime of the terminal session and is not
// affected by subsequent OSC 0/1/2 title writes from other applications,
// which is what makes it the right vehicle for cloister's persistent
// per-profile identity. Terminals that do not recognize OSC 1337 silently
// discard the sequence; this helper is therefore safe to call at any time,
// but callers should still gate on Capabilities.UserVars to avoid emitting
// unrecognized bytes on terminals that may render them literally.
//
// The value is base64-encoded as required by the OSC 1337 SetUserVar spec
// so values containing characters with special meaning to the parser
// (semicolons, control bytes, etc.) survive transit intact.
func setUserVar(key, value string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	fmt.Printf("\033]1337;SetUserVar=%s=%s\007", key, encoded)
}
