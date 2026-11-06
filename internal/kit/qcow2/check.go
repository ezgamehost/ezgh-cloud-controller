// Package qcow2 checks that a qcow2 disk image is self-contained and structurally sound before
// anything reads it as a disk: EZGH Images runs it on every upload, and blockstore's converter on
// every image it turns into a volume, so the two admit exactly the same set of files.
//
// Check reads the header, then walks the refcount table and the whole active L1/L2 tree through
// an io.ReaderAt, one cluster at a time. It refuses:
//
//   - anything that isn't qcow2 version 2 or 3 (ErrNotQcow2, ErrVersion);
//   - a backing file or a backing format (ErrBackingFile), which could point a hypervisor at
//     arbitrary host paths, and an external data file (ErrExternalDataFile);
//   - encryption (ErrEncrypted), unknown incompatible features (ErrUnknownIncompatible), extended
//     L2 entries (ErrExtendedL2), a cluster size outside 512 B to 2 MiB (ErrClusterSize), and a
//     compression type other than zlib or zstd (ErrCompression);
//   - a virtual size above the caller's cap (ErrTooLarge);
//   - any table, L2 table, data cluster or compressed extent outside the file (ErrOutOfBounds),
//     and any two of them sharing a cluster (ErrOverlap), except compressed extents, which qemu
//     packs into shared clusters and sectors;
//   - everything else qemu itself treats as corruption: the corrupt bit, reserved bits,
//     misaligned offsets, an L1 table too small for the virtual size (ErrCorrupt); and a
//     virtual size of 0, which is no disk (ErrCorrupt).
//
// Like qemu, it lets the last cluster run past the end of the file (by less than one cluster);
// a reader of a checked image reads those bytes as zeros. Tables (L1, L2, refcount) must lie
// wholly inside the file. Internal snapshots aren't walked: a converter reads only the active
// image. Refcounts aren't verified, since nothing reading the image uses them.
//
// Memory is bounded by one cluster of buffer (at most 2 MiB) plus two bits per cluster of the
// file.
package qcow2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Info describes a checked image.
type Info struct {
	Version     uint32 // 2 or 3
	VirtualSize uint64 // bytes: the smallest disk the image fits
	ClusterBits uint32 // 9 to 21
	Compression string // "zlib" or "zstd"
}

// The refusals. Every error Check returns wraps one of these, except a reader's own failure
// (other than reading past its end, which is ErrOutOfBounds), which it wraps instead.
var (
	ErrNotQcow2            = errors.New("qcow2: not a qcow2 image")
	ErrVersion             = errors.New("qcow2: unsupported version")
	ErrBackingFile         = errors.New("qcow2: backing files aren't allowed")
	ErrExternalDataFile    = errors.New("qcow2: external data files aren't allowed")
	ErrEncrypted           = errors.New("qcow2: encrypted images aren't allowed")
	ErrUnknownIncompatible = errors.New("qcow2: unknown incompatible feature")
	ErrExtendedL2          = errors.New("qcow2: extended L2 entries aren't supported")
	ErrClusterSize         = errors.New("qcow2: cluster size outside 512 B to 2 MiB")
	ErrCompression         = errors.New("qcow2: compression type isn't zlib or zstd")
	ErrTooLarge            = errors.New("qcow2: virtual size too large")
	ErrOutOfBounds         = errors.New("qcow2: reference outside the file")
	ErrOverlap             = errors.New("qcow2: overlapping clusters")
	ErrCorrupt             = errors.New("qcow2: corrupt image")
)

const (
	magic = 0x514649fb

	minClusterBits = 9
	maxClusterBits = 21

	v2HeaderLength = 72
	v3HeaderMin    = 104 // through header_length; the compression type byte follows

	maxL1Bytes       = 32 << 20 // qemu's QCOW_MAX_L1_SIZE
	maxRefTableBytes = 8 << 20  // qemu's QCOW_MAX_REFTABLE_SIZE
	maxSnapshots     = 65536    // qemu's QCOW_MAX_SNAPSHOTS
	maxRefcountOrder = 6

	incompatCorrupt     = 1 << 1
	incompatDataFile    = 1 << 2
	incompatCompression = 1 << 3
	incompatExtendedL2  = 1 << 4
	incompatKnown       = 1<<5 - 1

	compressionZlib = 0
	compressionZstd = 1

	extEnd            = 0x00000000
	extBackingFormat  = 0xe2792aca
	extCryptoHeader   = 0x0537be77
	extDataFileName   = 0x44415441
	sectorSize        = 512
	oflagCopied       = uint64(1) << 63
	oflagCompressed   = uint64(1) << 62
	oflagZero         = uint64(1)
	l1OffsetMask      = uint64(0x00fffffffffffe00)
	l1ReservedMask    = uint64(0x7f000000000001ff)
	l2OffsetMask      = uint64(0x00fffffffffffe00)
	l2ReservedMask    = uint64(0x3f000000000001fe)
	refTableReserved  = uint64(0x1ff)
	refTableOffsetMsk = ^refTableReserved
)

// Check validates the qcow2 image r, of fileSize bytes, and refuses a virtual size above
// maxVirtual. Its errors wrap the package's Err values (see the package comment).
func Check(r io.ReaderAt, fileSize int64, maxVirtual uint64) (Info, error) {
	if fileSize < 4 {
		return Info{}, fmt.Errorf("%w: %d bytes", ErrNotQcow2, max(fileSize, 0))
	}
	c := checker{r: r, size: uint64(fileSize)}
	h, err := c.header(maxVirtual)
	if err != nil {
		return Info{}, err
	}
	if err := c.walk(h); err != nil {
		return Info{}, err
	}
	compression := "zlib"
	if h.compression == compressionZstd {
		compression = "zstd"
	}
	return Info{Version: h.version, VirtualSize: h.virtualSize, ClusterBits: h.clusterBits, Compression: compression}, nil
}

type header struct {
	version, clusterBits uint32
	virtualSize          uint64
	l1Size               uint32
	l1Offset             uint64
	refTableOffset       uint64
	refTableClusters     uint32
	snapshots            uint32
	snapshotsOffset      uint64
	compression          uint8
	headerLength         uint64
}

type checker struct {
	r    io.ReaderAt
	size uint64

	clusterBits uint32
	cs          uint64
	// One bit per cluster of the file: used by a table or a data cluster, or by compressed
	// extents (which may share clusters with each other, and nothing else).
	used, compressed bitmap
}

func (c *checker) header(maxVirtual uint64) (header, error) {
	var b [112]byte
	n := min(c.size, uint64(len(b)))
	if err := c.read(b[:n], 0); err != nil {
		return header{}, err
	}
	if binary.BigEndian.Uint32(b[0:]) != magic {
		return header{}, ErrNotQcow2
	}
	if n < v2HeaderLength {
		return header{}, fmt.Errorf("%w: the file ends inside the header (%d bytes)", ErrOutOfBounds, c.size)
	}
	be32 := func(o int) uint32 { return binary.BigEndian.Uint32(b[o:]) }
	be64 := func(o int) uint64 { return binary.BigEndian.Uint64(b[o:]) }
	h := header{
		version:          be32(4),
		clusterBits:      be32(20),
		virtualSize:      be64(24),
		l1Size:           be32(36),
		l1Offset:         be64(40),
		refTableOffset:   be64(48),
		refTableClusters: be32(56),
		snapshots:        be32(60),
		snapshotsOffset:  be64(64),
		headerLength:     v2HeaderLength,
	}
	if h.version != 2 && h.version != 3 {
		return header{}, fmt.Errorf("%w: %d", ErrVersion, h.version)
	}
	if off := be64(8); off != 0 {
		return header{}, fmt.Errorf("%w: backing file name at %#x", ErrBackingFile, off)
	}
	if h.clusterBits < minClusterBits || h.clusterBits > maxClusterBits {
		return header{}, fmt.Errorf("%w: cluster bits %d", ErrClusterSize, h.clusterBits)
	}
	c.clusterBits, c.cs = h.clusterBits, uint64(1)<<h.clusterBits
	if m := be32(32); m != 0 {
		return header{}, fmt.Errorf("%w: crypt method %d", ErrEncrypted, m)
	}
	var incompat uint64
	if h.version == 3 {
		if n < v3HeaderMin {
			return header{}, fmt.Errorf("%w: the file ends inside the header (%d bytes)", ErrOutOfBounds, c.size)
		}
		incompat = be64(72)
		if order := be32(96); order > maxRefcountOrder {
			return header{}, fmt.Errorf("%w: refcount order %d", ErrCorrupt, order)
		}
		h.headerLength = uint64(be32(100))
		if h.headerLength < v3HeaderMin || h.headerLength%8 != 0 || h.headerLength > c.cs {
			return header{}, fmt.Errorf("%w: header length %d", ErrCorrupt, h.headerLength)
		}
		if h.headerLength > v3HeaderMin {
			if n <= v3HeaderMin {
				return header{}, fmt.Errorf("%w: the file ends inside the header (%d bytes)", ErrOutOfBounds, c.size)
			}
			h.compression = b[104]
		}
	}
	if unknown := incompat &^ incompatKnown; unknown != 0 {
		return header{}, fmt.Errorf("%w: bits %#x", ErrUnknownIncompatible, unknown)
	}
	if incompat&incompatCorrupt != 0 {
		return header{}, fmt.Errorf("%w: the corrupt bit is set", ErrCorrupt)
	}
	if incompat&incompatDataFile != 0 {
		return header{}, ErrExternalDataFile
	}
	if incompat&incompatExtendedL2 != 0 {
		return header{}, ErrExtendedL2
	}
	switch h.compression {
	case compressionZlib:
		if incompat&incompatCompression != 0 {
			return header{}, fmt.Errorf("%w: the compression type bit is set for zlib", ErrCompression)
		}
	case compressionZstd:
		if incompat&incompatCompression == 0 {
			return header{}, fmt.Errorf("%w: zstd without the compression type bit", ErrCompression)
		}
	default:
		return header{}, fmt.Errorf("%w: type %d", ErrCompression, h.compression)
	}
	if h.virtualSize > maxVirtual {
		return header{}, fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, h.virtualSize, maxVirtual)
	}
	if h.virtualSize == 0 {
		return header{}, fmt.Errorf("%w: virtual size 0", ErrCorrupt)
	}
	return h, c.extensions(h)
}

// extensions reads the header extensions, from the end of the header to the end of the first
// cluster, until the end marker. Unknown ones are skipped, as qemu skips them.
func (c *checker) extensions(h header) error {
	end := min(c.cs, c.size)
	if h.headerLength >= end {
		return nil
	}
	buf := make([]byte, end-h.headerLength)
	if err := c.read(buf, h.headerLength); err != nil {
		return err
	}
	for off := uint64(0); off < uint64(len(buf)); {
		if uint64(len(buf))-off < 8 {
			return fmt.Errorf("%w: header extension at %#x is cut off", ErrCorrupt, h.headerLength+off)
		}
		typ := binary.BigEndian.Uint32(buf[off:])
		length := uint64(binary.BigEndian.Uint32(buf[off+4:]))
		if typ == extEnd {
			return nil
		}
		if length > uint64(len(buf))-off-8 {
			return fmt.Errorf("%w: header extension %#x at %#x is too large", ErrCorrupt, typ, h.headerLength+off)
		}
		switch typ {
		case extBackingFormat:
			return fmt.Errorf("%w: a backing format extension", ErrBackingFile)
		case extDataFileName:
			return fmt.Errorf("%w: a data file name extension", ErrExternalDataFile)
		case extCryptoHeader:
			return fmt.Errorf("%w: an encryption header extension", ErrEncrypted)
		}
		off += 8 + (length+7)/8*8
	}
	return nil
}

func (c *checker) walk(h header) error {
	clusters := c.size>>c.clusterBits + 2 // the last, partial cluster and one past it
	c.used, c.compressed = newBitmap(clusters), newBitmap(clusters)
	if err := c.markTable(label{what: "the header"}, 0, h.headerLength); err != nil {
		return err
	}
	if err := c.refcounts(h); err != nil {
		return err
	}
	if h.snapshots > maxSnapshots {
		return fmt.Errorf("%w: %d snapshots", ErrCorrupt, h.snapshots)
	}
	if h.snapshots > 0 {
		if h.snapshotsOffset%c.cs != 0 {
			return fmt.Errorf("%w: snapshot table at %#x isn't cluster-aligned", ErrCorrupt, h.snapshotsOffset)
		}
		if h.snapshotsOffset >= c.size {
			return fmt.Errorf("%w: snapshot table at %#x", ErrOutOfBounds, h.snapshotsOffset)
		}
	}
	return c.l1(h)
}

// refcounts marks the refcount table and every refcount block it names.
func (c *checker) refcounts(h header) error {
	if h.refTableClusters == 0 {
		return fmt.Errorf("%w: no refcount table", ErrCorrupt)
	}
	if uint64(h.refTableClusters) > maxRefTableBytes/c.cs {
		return fmt.Errorf("%w: refcount table of %d clusters", ErrCorrupt, h.refTableClusters)
	}
	if h.refTableOffset%c.cs != 0 {
		return fmt.Errorf("%w: refcount table at %#x isn't cluster-aligned", ErrCorrupt, h.refTableOffset)
	}
	length := uint64(h.refTableClusters) * c.cs
	if err := c.markTable(label{what: "the refcount table"}, h.refTableOffset, length); err != nil {
		return err
	}
	return c.entries(h.refTableOffset, length, func(i, e uint64) error {
		if e&refTableReserved != 0 {
			return fmt.Errorf("%w: refcount table entry %d has reserved bits set", ErrCorrupt, i)
		}
		off := e & refTableOffsetMsk
		if off == 0 {
			return nil
		}
		if off%c.cs != 0 {
			return fmt.Errorf("%w: refcount block %d at %#x isn't cluster-aligned", ErrCorrupt, i, off)
		}
		return c.markTable(label{"refcount block %d", [2]uint64{i}, 1}, off, c.cs)
	})
}

// l1 walks the active L1 table and every L2 table it names, every entry (not only those inside
// the virtual size), so nothing the file references escapes the check.
func (c *checker) l1(h header) error {
	perL1 := c.cs * (c.cs / 8)
	needed := h.virtualSize / perL1
	if h.virtualSize%perL1 != 0 {
		needed++
	}
	if uint64(h.l1Size) < needed {
		return fmt.Errorf("%w: L1 table of %d entries, %d needed for the virtual size", ErrCorrupt, h.l1Size, needed)
	}
	if uint64(h.l1Size) > maxL1Bytes/8 {
		return fmt.Errorf("%w: L1 table of %d entries", ErrCorrupt, h.l1Size)
	}
	if h.l1Size == 0 {
		return nil
	}
	if h.l1Offset%c.cs != 0 {
		return fmt.Errorf("%w: L1 table at %#x isn't cluster-aligned", ErrCorrupt, h.l1Offset)
	}
	length := uint64(h.l1Size) * 8
	if err := c.markTable(label{what: "the L1 table"}, h.l1Offset, length); err != nil {
		return err
	}
	l2 := make([]byte, c.cs)
	return c.entries(h.l1Offset, length, func(i, e uint64) error {
		if e&l1ReservedMask != 0 {
			return fmt.Errorf("%w: L1 entry %d has reserved bits set", ErrCorrupt, i)
		}
		off := e & l1OffsetMask
		if off == 0 {
			return nil
		}
		if off%c.cs != 0 {
			return fmt.Errorf("%w: L2 table %d at %#x isn't cluster-aligned", ErrCorrupt, i, off)
		}
		if err := c.markTable(label{"L2 table %d", [2]uint64{i}, 1}, off, c.cs); err != nil {
			return err
		}
		if err := c.read(l2, off); err != nil {
			return err
		}
		for j := uint64(0); j < c.cs/8; j++ {
			if err := c.l2Entry(h, i, j, binary.BigEndian.Uint64(l2[j*8:])); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *checker) l2Entry(h header, i, j, e uint64) error {
	if e&oflagCompressed != 0 {
		if e&oflagCopied != 0 {
			return fmt.Errorf("%w: compressed L2 entry %d/%d has the copied flag", ErrCorrupt, i, j)
		}
		csizeShift := 62 - (c.clusterBits - 8)
		coffset := e & (uint64(1)<<csizeShift - 1)
		sectors := (e>>csizeShift)&(uint64(1)<<(c.clusterBits-8)-1) + 1
		end := coffset&^(sectorSize-1) + sectors*sectorSize
		if coffset >= c.size || (end > c.size && end-c.size >= c.cs) {
			return fmt.Errorf("%w: compressed L2 entry %d/%d at %#x (%d sectors), the file is %d bytes",
				ErrOutOfBounds, i, j, coffset, sectors, c.size)
		}
		for cl := coffset >> c.clusterBits; cl <= (end-1)>>c.clusterBits; cl++ {
			if c.used.get(cl) {
				return fmt.Errorf("%w: compressed L2 entry %d/%d at %#x shares cluster %#x", ErrOverlap, i, j, coffset, cl<<c.clusterBits)
			}
			c.compressed.set(cl)
		}
		return nil
	}
	if e&l2ReservedMask != 0 {
		return fmt.Errorf("%w: L2 entry %d/%d has reserved bits set", ErrCorrupt, i, j)
	}
	if e&oflagZero != 0 && h.version < 3 {
		return fmt.Errorf("%w: zero flag in a version 2 image, L2 entry %d/%d", ErrCorrupt, i, j)
	}
	off := e & l2OffsetMask
	if off == 0 {
		return nil // unallocated, or reads as zeros
	}
	if off%c.cs != 0 {
		return fmt.Errorf("%w: L2 entry %d/%d at %#x isn't cluster-aligned", ErrCorrupt, i, j, off)
	}
	if off >= c.size {
		return fmt.Errorf("%w: L2 entry %d/%d at %#x, the file is %d bytes", ErrOutOfBounds, i, j, off, c.size)
	}
	return c.mark(label{"L2 entry %d/%d", [2]uint64{i, j}, 2}, off>>c.clusterBits)
}

// markTable marks a table that must lie wholly inside the file.
func (c *checker) markTable(what label, off, length uint64) error {
	if off > c.size || length > c.size-off {
		return fmt.Errorf("%w: %s at %#x (%d bytes), the file is %d bytes", ErrOutOfBounds, what, off, length, c.size)
	}
	for cl := off >> c.clusterBits; cl <= (off+length-1)>>c.clusterBits; cl++ {
		if err := c.mark(what, cl); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) mark(what label, cluster uint64) error {
	if c.used.get(cluster) || c.compressed.get(cluster) {
		return fmt.Errorf("%w: %s uses cluster %#x twice", ErrOverlap, what, cluster<<c.clusterBits)
	}
	c.used.set(cluster)
	return nil
}

// entries calls f with each big-endian 64-bit entry of the table at off, reading a cluster at a
// time.
func (c *checker) entries(off, length uint64, f func(i, e uint64) error) error {
	buf := make([]byte, min(length, c.cs))
	for done := uint64(0); done < length; {
		chunk := buf[:min(length-done, uint64(len(buf)))]
		if err := c.read(chunk, off+done); err != nil {
			return err
		}
		for k := uint64(0); k < uint64(len(chunk))/8; k++ {
			if err := f(done/8+k, binary.BigEndian.Uint64(chunk[k*8:])); err != nil {
				return err
			}
		}
		done += uint64(len(chunk))
	}
	return nil
}

func (c *checker) read(buf []byte, off uint64) error {
	n, err := c.r.ReadAt(buf, int64(off))
	if n == len(buf) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: read %d of %d bytes at %#x", ErrOutOfBounds, n, len(buf), off)
	}
	return fmt.Errorf("qcow2: reading %d bytes at %#x: %w", len(buf), off, err)
}

// label names what a cluster belongs to, for an error message. It's formatted only when an error
// is returned, so walking a large image allocates nothing per cluster.
type label struct {
	what string
	args [2]uint64
	n    int
}

func (l label) String() string {
	switch l.n {
	case 1:
		return fmt.Sprintf(l.what, l.args[0])
	case 2:
		return fmt.Sprintf(l.what, l.args[0], l.args[1])
	}
	return l.what
}

type bitmap []uint64

func newBitmap(n uint64) bitmap    { return make(bitmap, (n+63)/64) }
func (b bitmap) get(i uint64) bool { return b[i/64]&(1<<(i%64)) != 0 }
func (b bitmap) set(i uint64)      { b[i/64] |= 1 << (i % 64) }
