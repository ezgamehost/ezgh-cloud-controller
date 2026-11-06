package logging

import "log/slog"

// Secret is a string that's never written intact: not in a log line, not by fmt, not by
// encoding/json. Reveal is the only way to its value.
type Secret string

func (Secret) LogValue() slog.Value         { return slog.StringValue(Redacted) }
func (Secret) String() string               { return Redacted }
func (Secret) GoString() string             { return Redacted }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (s Secret) Reveal() string             { return string(s) }
