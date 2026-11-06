package qcow2

import (
	"encoding/binary"
	"testing"
)

// opts shapes the image buildQcow2 writes. The zero value is a valid v3 image: 64 KiB clusters,
// a 1 GiB virtual size, two allocated data clusters, zlib.
type opts struct {
	Version     uint32 // 0 means 3
	ClusterBits uint32 // 0 means 16
	VirtualSize uint64 // 0 means 1 GiB

	// Compression is the header's compression type byte; nonzero also sets incompatible bit 3.
	Compression uint8
	// CompressedCluster adds two compressed extents packed into one host cluster, sharing a sector,
	// as qemu writes them.
	CompressedCluster bool
	// Unallocated leaves every L1 entry empty: no L2 table and no data clusters.
	Unallocated bool

	BackingFile  string // a backing file name in the header cluster
	ExternalData string // incompatible bit 2 and the external data file name extension
	CryptMethod  uint32
	Incompatible uint64 // ORed into the incompatible features

	L2EntryBeyondEOF bool // the second data cluster points far past the end of the file
	OverlapClusters  bool // the second data cluster is the first one again
}

// The layout, in clusters, when the L1 table fits in one cluster (as with the defaults).
const (
	clHeader    = 0
	clRefTable  = 1
	clRefBlock  = 2
	clL1        = 3
	clL2        = 4
	clData0     = 5
	clData1     = 6
	clCompHost  = 7
	defaultBits = 16
)

// at is a cluster's offset with the default cluster size.
func at(cluster int) uint64 { return uint64(cluster) << defaultBits }

// Offsets of single entries with the default cluster size, for tests that patch an image.
const (
	offL1Size          = 36
	offCompressionType = 104
)

func l1Entry(i int) int { return int(at(clL1)) + 8*i }
func l2Entry(i int) int { return int(at(clL2)) + 8*i }

func put64(b []byte, off int, v uint64) { binary.BigEndian.PutUint64(b[off:], v) }
func get64(b []byte, off int) uint64    { return binary.BigEndian.Uint64(b[off:]) }

const copied = uint64(1) << 63

// buildQcow2 writes a qcow2 image in memory, with exact refcounts, so that `qemu-img check` finds
// nothing wrong with the valid ones.
func buildQcow2(t testing.TB, o opts) []byte {
	t.Helper()
	version := o.Version
	if version == 0 {
		version = 3
	}
	bits := o.ClusterBits
	if bits == 0 {
		bits = defaultBits
	}
	virtual := o.VirtualSize
	if virtual == 0 {
		virtual = 1 << 30
	}
	cs := uint64(1) << bits
	perL1 := cs * (cs / 8)
	l1Size := virtual / perL1
	if virtual%perL1 != 0 {
		l1Size++
	}
	if l1Size > 1<<16 || l1Size < 2 {
		l1Size = 2 // only the virtual-size refusals see this
	}
	// The L1 table may span clusters; everything after it moves along. With the defaults it's one.
	shift := int((l1Size*8+cs-1)/cs) - 1
	l2At, data0, data1, compHost := clL2+shift, clData0+shift, clData1+shift, clCompHost+shift
	clusters := data1 + 1
	switch {
	case o.Unallocated:
		clusters = l2At
	case o.CompressedCluster:
		clusters = compHost + 1
	}
	if uint64(clusters) > cs/2 {
		t.Fatalf("buildQcow2: %d clusters need more than one refcount block", clusters)
	}
	img := make([]byte, uint64(clusters)*cs)
	off := func(c int) uint64 { return uint64(c) * cs }

	// Header.
	be32 := func(o int, v uint32) { binary.BigEndian.PutUint32(img[o:], v) }
	be64 := func(o int, v uint64) { binary.BigEndian.PutUint64(img[o:], v) }
	be32(0, 0x514649fb)
	be32(4, version)
	be32(20, bits)
	be64(24, virtual)
	be32(32, o.CryptMethod)
	be32(36, uint32(l1Size))
	be64(40, off(clL1))
	be64(48, off(clRefTable))
	be32(56, 1)
	headerEnd := 72
	if version >= 3 {
		incompat := o.Incompatible
		if o.Compression != 0 {
			incompat |= 1 << 3
		}
		if o.ExternalData != "" {
			incompat |= 1 << 2
		}
		be64(72, incompat)
		be32(96, 4) // refcount order: 16-bit refcounts
		be32(100, 112)
		img[104] = o.Compression
		headerEnd = 112
	}
	if o.ExternalData != "" {
		be32(headerEnd, 0x44415441)
		be32(headerEnd+4, uint32(len(o.ExternalData)))
		copy(img[headerEnd+8:], o.ExternalData)
		headerEnd += 8 + (len(o.ExternalData)+7)/8*8
	}
	// The end-of-extensions marker is the zeros that follow.
	if o.BackingFile != "" {
		be64(8, 0x400)
		be32(16, uint32(len(o.BackingFile)))
		copy(img[0x400:], o.BackingFile)
	}

	// Refcount table and block: every cluster is used once, the compressed host cluster twice.
	be64(int(off(clRefTable)), off(clRefBlock))
	for c := 0; c < clusters; c++ {
		n := uint16(1)
		if c == compHost && o.CompressedCluster {
			n = 2
		}
		binary.BigEndian.PutUint16(img[off(clRefBlock)+uint64(2*c):], n)
	}
	if o.Unallocated {
		return img
	}

	// L1 and L2.
	be64(int(off(clL1)), off(l2At)|copied)
	l2 := int(off(l2At))
	be64(l2, off(data0)|copied)
	second := off(data1)
	switch {
	case o.L2EntryBeyondEOF:
		second = 1 << 40
	case o.OverlapClusters:
		second = off(data0)
	}
	be64(l2+8, second|copied)
	for i := off(data0); i < off(data1)+cs; i++ {
		img[i] = byte(i>>9) | 1
	}
	if o.CompressedCluster {
		// Two extents as qemu packs them: 700 bytes at the cluster's start, then 300 bytes, the
		// second starting in the first's last sector. Each descriptor counts the sectors it spans
		// beyond its first.
		csizeShift := 62 - (bits - 8)
		host := off(compHost)
		compressed := func(coffset, length uint64) uint64 {
			extra := (coffset+length-1)>>9 - coffset>>9
			return 1<<62 | extra<<csizeShift | coffset
		}
		be64(l2+16, compressed(host, 700))
		be64(l2+24, compressed(host+700, 300))
		for i := host; i < host+1000; i++ {
			img[i] = byte(i) | 0x80
		}
	}
	return img
}
