package qcow2

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const maxVirtual = 1 << 38 // 256 GiB, images' cap

// acceptFixtures are valid images, all with a 1 GiB virtual size.
func acceptFixtures(t testing.TB) map[string][]byte {
	semi := buildQcow2(t, opts{})
	return map[string][]byte{
		"v3 64k":                  buildQcow2(t, opts{}),
		"v2":                      buildQcow2(t, opts{Version: 2}),
		"zstd compressed cluster": buildQcow2(t, opts{Compression: 1, CompressedCluster: true}),
		"zlib compressed cluster": buildQcow2(t, opts{CompressedCluster: true}),
		"sparse":                  buildQcow2(t, opts{Unallocated: true}),
		"2 MiB clusters":          buildQcow2(t, opts{ClusterBits: 21}),
		// qemu allows the last cluster to be only partly in the file; the rest reads as zeros.
		"semi-allocated last cluster": semi[:len(semi)-0x8000],
	}
}

func TestCheckAccepts(t *testing.T) {
	for name, img := range acceptFixtures(t) {
		info, err := Check(bytes.NewReader(img), int64(len(img)), maxVirtual)
		if err != nil || info.VirtualSize != 1<<30 {
			t.Errorf("%s: %+v %v", name, info, err)
		}
	}
}

// 512-byte clusters with an L1 table spanning two clusters.
func TestCheckAcceptsSmallClusters(t *testing.T) {
	img := buildQcow2(t, opts{ClusterBits: 9, VirtualSize: 4 << 20})
	info, err := Check(bytes.NewReader(img), int64(len(img)), maxVirtual)
	if err != nil || info.ClusterBits != 9 || info.VirtualSize != 4<<20 {
		t.Errorf("%+v %v", info, err)
	}
}

func TestCheckInfo(t *testing.T) {
	cases := map[string]struct {
		img  []byte
		want Info
	}{
		"v3":   {buildQcow2(t, opts{}), Info{Version: 3, VirtualSize: 1 << 30, ClusterBits: 16, Compression: "zlib"}},
		"v2":   {buildQcow2(t, opts{Version: 2}), Info{Version: 2, VirtualSize: 1 << 30, ClusterBits: 16, Compression: "zlib"}},
		"zstd": {buildQcow2(t, opts{Compression: 1}), Info{Version: 3, VirtualSize: 1 << 30, ClusterBits: 16, Compression: "zstd"}},
	}
	for name, c := range cases {
		got, err := Check(bytes.NewReader(c.img), int64(len(c.img)), maxVirtual)
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, c.want)
		}
	}
}

// A dirty image (lazy refcounts not yet written back) reads correctly: refcounts aren't used.
func TestCheckAcceptsDirty(t *testing.T) {
	img := buildQcow2(t, opts{Incompatible: 1 << 0})
	if _, err := Check(bytes.NewReader(img), int64(len(img)), maxVirtual); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefuses(t *testing.T) {
	cases := map[string]struct {
		img  []byte
		want error
	}{
		"raw":                {make([]byte, 1<<20), ErrNotQcow2},
		"empty":              {nil, ErrNotQcow2},
		"version 4":          {buildQcow2(t, opts{Version: 4}), ErrVersion},
		"backing file":       {buildQcow2(t, opts{BackingFile: "/etc/shadow"}), ErrBackingFile},
		"external data file": {buildQcow2(t, opts{ExternalData: "x.raw"}), ErrExternalDataFile},
		"encrypted":          {buildQcow2(t, opts{CryptMethod: 1}), ErrEncrypted},
		"unknown incompat":   {buildQcow2(t, opts{Incompatible: 1 << 9}), ErrUnknownIncompatible},
		"extended L2":        {buildQcow2(t, opts{Incompatible: 1 << 4}), ErrExtendedL2},
		"cluster bits 22":    {buildQcow2(t, opts{ClusterBits: 22}), ErrClusterSize},
		"cluster bits 8":     {patched(t, opts{}, func(b []byte) { b[23] = 8 }), ErrClusterSize},
		"compression 2":      {buildQcow2(t, opts{Compression: 2}), ErrCompression},
		"virtual 2^63":       {buildQcow2(t, opts{VirtualSize: 1 << 63}), ErrTooLarge},
		"L2 past EOF":        {buildQcow2(t, opts{L2EntryBeyondEOF: true}), ErrOutOfBounds},
		"overlap":            {buildQcow2(t, opts{OverlapClusters: true}), ErrOverlap},
		"truncated table":    {buildQcow2(t, opts{})[:0x30000], ErrOutOfBounds},

		"truncated header": {buildQcow2(t, opts{})[:64], ErrOutOfBounds},
		"corrupt bit":      {buildQcow2(t, opts{Incompatible: 1 << 1}), ErrCorrupt},
		"zstd type without its incompatible bit": {
			patched(t, opts{}, func(b []byte) { b[offCompressionType] = 1 }), ErrCompression},
		"L1 too small for the virtual size": {
			patched(t, opts{}, func(b []byte) { b[offL1Size+3] = 1 }), ErrCorrupt},
		"L1 entry reserved bits": {
			patched(t, opts{}, func(b []byte) { put64(b, l1Entry(0), get64(b, l1Entry(0))|1<<60) }), ErrCorrupt},
		"L2 entry reserved bits": {
			patched(t, opts{}, func(b []byte) { put64(b, l2Entry(0), get64(b, l2Entry(0))|1<<3) }), ErrCorrupt},
		"unaligned L2 table": {
			patched(t, opts{}, func(b []byte) { put64(b, l1Entry(0), (at(clL2)+0x200)|copied) }), ErrCorrupt},
		"unaligned data cluster": {
			patched(t, opts{}, func(b []byte) { put64(b, l2Entry(0), (at(clData0)+0x200)|copied) }), ErrCorrupt},
		"L2 table past EOF": {
			patched(t, opts{}, func(b []byte) { put64(b, l1Entry(1), at(64)|copied) }), ErrOutOfBounds},
		"L2 table shared by two L1 entries": {
			patched(t, opts{}, func(b []byte) { put64(b, l1Entry(1), get64(b, l1Entry(0))) }), ErrOverlap},
		"data cluster on the L1 table": {
			patched(t, opts{}, func(b []byte) { put64(b, l2Entry(0), at(clL1)|copied) }), ErrOverlap},
		"data cluster on the refcount table": {
			patched(t, opts{}, func(b []byte) { put64(b, l2Entry(0), at(clRefTable)|copied) }), ErrOverlap},
		"zero flag in a v2 image": {
			patched(t, opts{Version: 2}, func(b []byte) { put64(b, l2Entry(0), get64(b, l2Entry(0))|1) }), ErrCorrupt},
		"compressed extent with the copied flag": {
			patched(t, opts{CompressedCluster: true}, func(b []byte) { put64(b, l2Entry(2), get64(b, l2Entry(2))|copied) }), ErrCorrupt},
		"compressed extent inside a data cluster": {
			patched(t, opts{CompressedCluster: true}, func(b []byte) { put64(b, l2Entry(2), 1<<62|at(clData0)) }), ErrOverlap},
		"compressed extent past EOF": {
			patched(t, opts{CompressedCluster: true}, func(b []byte) { put64(b, l2Entry(2), 1<<62|at(64)) }), ErrOutOfBounds},
		"refcount table past EOF": {
			patched(t, opts{}, func(b []byte) { put64(b, 48, at(64)) }), ErrOutOfBounds},
		"refcount block past EOF": {
			patched(t, opts{}, func(b []byte) { put64(b, int(at(clRefTable)), at(64)) }), ErrOutOfBounds},
		"virtual size 0": {
			patched(t, opts{}, func(b []byte) { put64(b, 24, 0) }), ErrCorrupt},
		"no refcount table": {
			patched(t, opts{}, func(b []byte) { b[59] = 0 }), ErrCorrupt},
	}
	for name, c := range cases {
		if _, err := Check(bytes.NewReader(c.img), int64(len(c.img)), maxVirtual); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
}

func TestCheckVirtualSizeCap(t *testing.T) {
	img := buildQcow2(t, opts{})
	if _, err := Check(bytes.NewReader(img), int64(len(img)), 1<<30); err != nil {
		t.Errorf("at the cap: %v", err)
	}
	if _, err := Check(bytes.NewReader(img), int64(len(img)), 1<<30-1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the cap: %v", err)
	}
}

// A ReaderAt shorter than the fileSize it came with is out of bounds, not a panic or a hang.
func TestCheckShortReader(t *testing.T) {
	img := buildQcow2(t, opts{})
	if _, err := Check(bytes.NewReader(img[:0x48000]), int64(len(img)), maxVirtual); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("got %v", err)
	}
}

func patched(t testing.TB, o opts, f func([]byte)) []byte {
	img := buildQcow2(t, o)
	f(img)
	return img
}

// qemu-img agrees that the accepted images are valid, and reads the same virtual and cluster
// sizes. CI's kit job sets QCOW2_DIFF=1 with qemu-utils installed.
func TestCheckDifferentialQemuImg(t *testing.T) {
	if os.Getenv("QCOW2_DIFF") != "1" {
		t.Skip("QCOW2_DIFF=1 runs qemu-img against the accepted fixtures")
	}
	dir := t.TempDir()
	for name, img := range acceptFixtures(t) {
		path := filepath.Join(dir, "image.qcow2")
		if err := os.WriteFile(path, img, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("qemu-img", "check", "--output=json", "-f", "qcow2", path).Output()
		if err != nil {
			t.Errorf("%s: qemu-img check: %v\n%s", name, err, out)
			continue
		}
		var check map[string]any
		if err := json.Unmarshal(out, &check); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		for _, k := range []string{"check-errors", "corruptions", "leaks"} {
			if n, ok := check[k].(float64); ok && n != 0 {
				t.Errorf("%s: qemu-img check: %s = %v\n%s", name, k, n, out)
			}
		}
		out, err = exec.Command("qemu-img", "info", "--output=json", "-f", "qcow2", path).Output()
		if err != nil {
			t.Errorf("%s: qemu-img info: %v\n%s", name, err, out)
			continue
		}
		var info struct {
			VirtualSize uint64 `json:"virtual-size"`
			ClusterSize uint64 `json:"cluster-size"`
		}
		if err := json.Unmarshal(out, &info); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		got, err := Check(bytes.NewReader(img), int64(len(img)), maxVirtual)
		if err != nil || got.VirtualSize != info.VirtualSize || uint64(1)<<got.ClusterBits != info.ClusterSize {
			t.Errorf("%s: Check %+v %v; qemu-img info %+v", name, got, err, info)
		}
	}
}
