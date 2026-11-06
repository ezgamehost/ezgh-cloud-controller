package secretsapi

import (
	"encoding/json"
	"testing"
)

func TestValueJSON(t *testing.T) {
	var got map[string]Value
	if err := json.Unmarshal([]byte(`{"A":"text","B":{"base64":"/wD+"}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got["A"].Binary || got["A"].Text != "text" || !got["B"].Binary {
		t.Fatalf("%+v", got)
	}
	if b, _ := got["B"].Bytes(); string(b) != "\xff\x00\xfe" {
		t.Errorf("bytes %q", b)
	}
	out, _ := json.Marshal(got)
	if string(out) != `{"A":"text","B":{"base64":"/wD+"}}` {
		t.Errorf("marshal %s", out)
	}
	if err := json.Unmarshal([]byte(`{"x":1}`), &got); err == nil {
		t.Error("a number is not a value")
	}
	if v := NewValue([]byte{0xff}); !v.Binary || v.Base64 != "/w==" {
		t.Errorf("NewValue %+v", v)
	}
	if v := NewValue([]byte("héllo")); v.Binary || v.Text != "héllo" {
		t.Errorf("NewValue %+v", v)
	}
}
