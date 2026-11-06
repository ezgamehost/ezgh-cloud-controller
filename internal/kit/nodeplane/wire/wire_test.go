package wire

import (
	"reflect"
	"testing"
)

// Two frames of a made-up protocol: the codec knows nothing of any product's.
type hello struct {
	Agent string `json:"agent"`
	Seal  []byte `json:"seal"`
}

type status struct {
	Epoch int64             `json:"epoch"`
	Tags  map[string]string `json:"tags"`
}

func (hello) Type() string  { return "hello" }
func (status) Type() string { return "status" }

var codec = NewCodec(hello{}, status{})

func TestFrameRoundTrip(t *testing.T) {
	for i, m := range []Message{&hello{Agent: "0.1.0", Seal: []byte{1, 2}}, &status{Epoch: 3, Tags: map[string]string{"a": "b"}}} {
		data, err := Encode(int64(i+1), m)
		if err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		f, err := codec.Decode(data)
		if err != nil {
			t.Fatalf("%T: %v", m, err)
		}
		if f.Seq != int64(i+1) || !reflect.DeepEqual(f.Message, m) {
			t.Errorf("%T: got %+v", m, f)
		}
	}
}

// A frame's shape on the wire is the contract with images already running.
func TestWireShape(t *testing.T) {
	data, err := Encode(7, hello{Agent: "x", Seal: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"seq":7,"type":"hello","body":{"agent":"x","seal":"AQ=="}}`; string(data) != want {
		t.Errorf("got %s", data)
	}
}

func TestUnknownFieldIgnored(t *testing.T) {
	f, err := codec.Decode([]byte(`{"seq":1,"new":true,"type":"status","body":{"epoch":5,"new":{"a":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Message.(*status); got.Epoch != 5 {
		t.Errorf("got %+v", got)
	}
}

// A sender newer than its reader can send frames the reader has never heard of.
func TestUnknownFrameSkipped(t *testing.T) {
	f, err := codec.Decode([]byte(`{"seq":2,"type":"somethingNew","body":{"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := f.Message.(Unknown); !ok || got.Type() != "somethingNew" || f.Seq != 2 {
		t.Errorf("got %+v", f)
	}
}

// A missing body would otherwise decode as a frame of zero values.
func TestMissingBodyIsAnError(t *testing.T) {
	for _, data := range []string{`{"seq":1,"type":"status"}`, `{"seq":1,"type":"status","body":null}`, `{"seq":1,"body":{}}`, `not json`} {
		if f, err := codec.Decode([]byte(data)); err == nil {
			t.Errorf("%s: got %+v", data, f)
		}
	}
}

func TestNewCodecRefusesDuplicateTypes(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("two frames of one type were accepted")
		}
	}()
	NewCodec(hello{}, &hello{})
}
