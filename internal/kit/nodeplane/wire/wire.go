// Package wire is the envelope of a node plane: sequenced JSON frames over one WebSocket per node,
// between a product's management service and the agent on its hidden VMs. Each product keeps its
// own frame types (its agentproto or nodeproto) and builds a Codec from them.
//
// Nodes run pinned images for days, so the control plane can always be newer than the agent it
// talks to, and a protocol only grows:
//
//   - Add fields and frame types; never rename, retype or repurpose one. A reader ignores unknown
//     fields, and Decode hands back unknown frame types as Unknown, for the caller to skip.
//   - An older image ignores a field it doesn't know, so a field that restricts something waits
//     until the agent's version shows it enforces it.
//
// A raw JSON field left nil arrives as the literal null, not as empty: unmarshal it, don't len it.
//
// Each frame's seq is its sender's, rising by one on a connection.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Message is a frame's body.
type Message interface {
	// Type is the frame's type on the wire.
	Type() string
}

// Unknown is a frame of a type this build doesn't know: skip it.
type Unknown string

func (u Unknown) Type() string { return string(u) }

// Frame is one decoded frame. Message is a pointer to one of the Codec's frame types, or an Unknown.
type Frame struct {
	Seq     int64
	Message Message
}

type envelope struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

// Codec decodes one protocol's frames.
type Codec struct {
	types map[string]reflect.Type
}

// NewCodec knows frames, given as values or pointers of each type. Two frames of one Type panic: a
// protocol is fixed at build time.
func NewCodec(frames ...Message) *Codec {
	c := &Codec{types: map[string]reflect.Type{}}
	for _, m := range frames {
		t := reflect.TypeOf(m)
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if _, dup := c.types[m.Type()]; dup {
			panic("wire: frame type " + m.Type() + " given twice")
		}
		c.types[m.Type()] = t
	}
	return c
}

// Encode is one frame's bytes.
func Encode(seq int64, m Message) ([]byte, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{Seq: seq, Type: m.Type(), Body: body})
}

// Decode reads one frame. An unknown type isn't an error, and unknown fields are ignored; anything
// else that doesn't parse is.
func (c *Codec) Decode(data []byte) (Frame, error) {
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Frame{}, err
	}
	if e.Type == "" {
		return Frame{}, errors.New("wire: a frame with no type")
	}
	t, ok := c.types[e.Type]
	if !ok {
		return Frame{Seq: e.Seq, Message: Unknown(e.Type)}, nil
	}
	// A missing or null body would otherwise decode as a frame of zero values.
	if len(e.Body) == 0 || string(e.Body) == "null" {
		return Frame{}, fmt.Errorf("wire: %s: a frame with no body", e.Type)
	}
	v := reflect.New(t).Interface()
	if err := json.Unmarshal(e.Body, v); err != nil {
		return Frame{}, fmt.Errorf("wire: %s: %w", e.Type, err)
	}
	return Frame{Seq: e.Seq, Message: v.(Message)}, nil
}
