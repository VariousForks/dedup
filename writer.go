package dedup

import (
	"bytes"
	hasher "crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"runtime"
	"sync"

	"github.com/klauspost/dedup/sort"
)

type Writer interface {
	io.WriteCloser

	// Split content, so a new block begins with next write.
	Split()

	// MemUse returns an approximate maximum memory use in bytes for
	// encoder (Writer) and decoder (Reader) for the given number of bytes.
	MemUse(bytes int) (encoder, decoder int64)

	// Returns the current number of blocks.
	// Blocks may still be processing.
	Blocks() int
}

// Size of the underlying hash in bytes for those interested.
const HashSize = hasher.Size

// The smallest "maximum" block size allowed.
const MinBlockSize = 512

// ErrMaxMemoryTooSmall is returned if the encoder isn't allowed to store
// even 1 block.
var ErrMaxMemoryTooSmall = errors.New("there must be at be space for 1 block")

// Deduplication mode used to determine how input is split.
type Mode int

const (
	// Fixed block size
	//
	// This is by far the fastest mode, and checks for duplicates
	// In fixed block sizes.
	// It can be helpful to use the "Split" function to reset offset, which
	// will reset duplication search at the position you are at.
	ModeFixed Mode = 0

	// Dynamic block size.
	//
	// This mode will create a deduplicator that will split the contents written
	// to it into dynamically sized blocks.
	// The size given indicates the maximum block size. Average size is usually maxSize/4.
	// Minimum block size is maxSize/64.
	ModeDynamic = 1

	// Dynamic block size.
	//
	// This mode will create a deduplicator that will split the contents written
	// to it into dynamically sized blocks.
	// The size given indicates the maximum block size. Average size is usually maxSize/4.
	// Minimum block size is maxSize/64.
	ModeDynamicEntropy = 2
)

// Fragment is a file fragment.
// It is the data returned by the NewSplitter.
type Fragment struct {
	Hash    [HashSize]byte // Hash of the fragment
	Payload []byte         // Data of the fragment.
	New     bool           // Will be true, if the data hasn't been encountered before.
	N       uint           // Sequencially incrementing number for each segment.
}

type writer struct {
	blks      io.Writer                          // Block data writer
	idx       io.Writer                          // Index writer
	frags     chan<- Fragment                    // Fragment output
	maxSize   int                                // Maximum Block size
	maxBlocks int                                // Maximum backreference distance
	index     map[[hasher.Size]byte]int          // Known hashes and their index
	input     chan *block                        // Channel containing blocks to be hashed
	write     chan *block                        // Channel containing (ordered) blocks to be written
	exited    chan struct{}                      // Closed when the writer exits.
	cur       []byte                             // Current block being written
	off       int                                // Write offset in current block
	buffers   chan *block                        // Buffers ready for re-use.
	vari64    []byte                             // Temporary buffer for writing varints
	err       error                              // Error state
	mu        sync.Mutex                         // Mutex for error state
	nblocks   int                                // Current block number. First block is 1.
	writer    func(*writer, []byte) (int, error) // Writes are forwarded here.
	flush     func(*writer) error                // Called from Close *before* the writer is closed.
	close     func(*writer) error                // Called from Close *after* the writer is closed.
	split     func(*writer)                      // Called when Split is called.
}

// block contains information about a single block
type block struct {
	data     []byte
	sha1Hash [hasher.Size]byte
	hashDone chan error
	N        int
}

// ErrSizeTooSmall is returned if the requested block size is smaller than
// hash size.
var ErrSizeTooSmall = errors.New("maximum block size too small. must be at least 512 bytes")

// NewWriter will create a deduplicator that will split the contents written
// to it into blocks and de-duplicate these.
//
// The output is delivered as two streams, an index stream and a block stream.
//
// The index stream will contain information about which blocks are deduplicated
// and the block stream will contain uncompressed data blocks.
//
// You can set the maximum memory for the decoder to use.
// This limits the length a match can be made.
// This is very conservative, so you can set this at the absolute limit of memory available.
// If you use dynamic blocks, also note that the average size is 1/4th of the maximum block size.
// Set maxMemory to 0 to disable decoder memory limit.
//
// This function returns data that is compatible with the NewReader function.
// The returned writer must be closed to flush the remaining data.
func NewWriter(index io.Writer, blocks io.Writer, mode Mode, maxSize, maxMemory uint) (Writer, error) {
	ncpu := runtime.GOMAXPROCS(0)
	// For small block sizes we need to keep a pretty big buffer to keep input fed.
	// Constant below appears to be sweet spot measured with 4K blocks.
	var bufmul = 256 << 10 / int(maxSize)
	if bufmul < 2 {
		bufmul = 2
	}

	w := &writer{
		blks:      blocks,
		idx:       index,
		maxSize:   int(maxSize),
		index:     make(map[[hasher.Size]byte]int),
		input:     make(chan *block, ncpu*bufmul),
		write:     make(chan *block, ncpu*bufmul),
		exited:    make(chan struct{}, 0),
		cur:       make([]byte, maxSize),
		vari64:    make([]byte, binary.MaxVarintLen64),
		buffers:   make(chan *block, ncpu*bufmul),
		nblocks:   1,
		maxBlocks: int(maxMemory / maxSize),
	}

	switch mode {
	case ModeFixed:
		fw := &fixedWriter{}
		w.writer = fw.write
		w.split = fw.split
	case ModeDynamic:
		zw := newZpaqWriter(maxSize)
		w.writer = zw.write
		w.split = zw.split
	case ModeDynamicEntropy:
		zw := newEntropyWriter(maxSize)
		w.writer = zw.write
		w.split = zw.split
	default:
		return nil, fmt.Errorf("dedup: unknown mode")
	}

	if w.maxSize < MinBlockSize {
		return nil, ErrSizeTooSmall
	}

	w.close = idxClose
	w.putUint64(1)               // Format
	w.putUint64(uint64(maxSize)) // Maximum block size

	// Start one goroutine per core
	for i := 0; i < ncpu; i++ {
		go w.hasher()
	}
	// Insert the buffers we will use
	for i := 0; i < ncpu*bufmul; i++ {
		w.buffers <- &block{data: make([]byte, maxSize), hashDone: make(chan error, 1)}
	}
	go w.blockWriter()
	return w, nil
}

// NewStreamWriter will create a deduplicator that will split the contents written
// to it into blocks and de-duplicate these.
//
// The output is delivered as a single stream, and memory use will remain stable for
// both writing and reading the stream.
//
// This function returns data that is compatible with the NewStreamReader function.
//
// You can must set the maximum memory for the decoder to use.
// This limits the length a match can be made.
// If you use dynamic blocks, also note that the average size is 1/4th of the maximum block size.
//
// The returned writer must be closed to flush the remaining data.
func NewStreamWriter(out io.Writer, mode Mode, maxSize, maxMemory uint) (Writer, error) {
	ncpu := runtime.GOMAXPROCS(0)
	// For small block sizes we need to keep a pretty big buffer to keep input fed.
	// Constant below appears to be sweet spot measured with 4K blocks.
	var bufmul = 256 << 10 / int(maxSize)
	if bufmul < 2 {
		bufmul = 2
	}
	if maxMemory < maxSize {
		return nil, ErrMaxMemoryTooSmall
	}
	w := &writer{
		idx:       out,
		maxSize:   int(maxSize),
		index:     make(map[[hasher.Size]byte]int),
		input:     make(chan *block, ncpu*bufmul),
		write:     make(chan *block, ncpu*bufmul),
		exited:    make(chan struct{}, 0),
		cur:       make([]byte, maxSize),
		vari64:    make([]byte, binary.MaxVarintLen64),
		buffers:   make(chan *block, ncpu*bufmul),
		nblocks:   1,
		maxBlocks: int(maxMemory / maxSize),
	}

	switch mode {
	case ModeFixed:
		fw := &fixedWriter{}
		w.writer = fw.write
	case ModeDynamic:
		zw := newZpaqWriter(maxSize)
		w.writer = zw.write
	case ModeDynamicEntropy:
		zw := newEntropyWriter(maxSize)
		w.writer = zw.write
		/*	case ModeDynamicSignatures:
				zw := newZpaqWriter(maxSize)
				w.writer = zw.writeFile
			case ModeSignaturesOnly:
				w.writer = fileSplitOnly
		*/
	default:
		return nil, fmt.Errorf("dedup: unknown mode")
	}

	if w.maxSize < MinBlockSize {
		return nil, ErrSizeTooSmall
	}

	w.close = streamClose
	w.putUint64(2)                   // Format
	w.putUint64(uint64(maxSize))     // Maximum block size
	w.putUint64(uint64(w.maxBlocks)) // Maximum backreference length

	// Start one goroutine per core
	for i := 0; i < ncpu; i++ {
		go w.hasher()
	}
	// Insert the buffers we will use
	for i := 0; i < ncpu*bufmul; i++ {
		w.buffers <- &block{data: make([]byte, maxSize), hashDone: make(chan error, 1)}
	}
	go w.blockStreamWriter()
	return w, nil
}

// NewSplitter will return a writer you can write data to,
// and the file will be split into separate fragments.
//
// You must supply a fragment channel, that will output fragments for
// the data you have written. The channel must accept data while you
// write to the spliter.
//
// For each fragment the SHA-1 hash of the data section is returned,
// along with the raw data of this segment.
//
// When you call Close on the returned Writer, the final fragments
// will be sent and the channel will be closed.
func NewSplitter(fragments chan<- Fragment, mode Mode, maxSize uint) (Writer, error) {
	ncpu := runtime.GOMAXPROCS(0)
	// For small block sizes we need to keep a pretty big buffer to keep input fed.
	// Constant below appears to be sweet spot measured with 4K blocks.
	var bufmul = 256 << 10 / int(maxSize)
	if bufmul < 2 {
		bufmul = 2
	}

	w := &writer{
		frags:   fragments,
		maxSize: int(maxSize),
		index:   make(map[[hasher.Size]byte]int),
		input:   make(chan *block, ncpu*bufmul),
		write:   make(chan *block, ncpu*bufmul),
		exited:  make(chan struct{}, 0),
		cur:     make([]byte, maxSize),
		vari64:  make([]byte, binary.MaxVarintLen64),
		buffers: make(chan *block, ncpu*bufmul),
		nblocks: 1,
	}

	switch mode {
	case ModeFixed:
		fw := &fixedWriter{}
		w.writer = fw.write
		w.split = fw.split
	case ModeDynamic:
		zw := newZpaqWriter(maxSize)
		w.writer = zw.write
		w.split = zw.split
	case ModeDynamicEntropy:
		zw := newEntropyWriter(maxSize)
		w.writer = zw.write
		w.split = zw.split
	default:
		return nil, fmt.Errorf("dedup: unknown mode")
	}

	w.flush = func(w *writer) error {
		w.split(w)
		return w.err
	}

	if w.maxSize < MinBlockSize {
		return nil, ErrSizeTooSmall
	}

	// Start one goroutine per core
	for i := 0; i < ncpu; i++ {
		go w.hasher()
	}
	// Insert the buffers we will use
	for i := 0; i < ncpu*bufmul; i++ {
		w.buffers <- &block{data: make([]byte, maxSize), hashDone: make(chan error, 1)}
	}
	go w.fragmentWriter()
	return w, nil
}

// putUint64 will Write a uint64 value to index stream.
func (w *writer) putUint64(v uint64) error {
	n := binary.PutUvarint(w.vari64, v)
	n2, err := w.idx.Write(w.vari64[:n])
	if err != nil {
		return err
	}
	if n2 != n {
		return io.ErrShortWrite
	}
	return nil
}

// Split content, so a new block begins with next write
func (w *writer) Split() {
	w.split(w)
}

func (w *writer) Blocks() int {
	w.mu.Lock()
	b := w.nblocks - 1
	w.mu.Unlock()
	return b
}

// Write contents to the deduplicator.
func (w *writer) Write(b []byte) (n int, err error) {
	w.mu.Lock()
	err = w.err
	w.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return w.writer(w, b)
}

// setErr will set the error state of the writer.
func (w *writer) setErr(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// idxClose will flush the remainder of an index based stream
func idxClose(w *writer) (err error) {
	// Insert length of remaining data into index
	w.putUint64(uint64(math.MaxUint64))
	w.putUint64(uint64(w.maxSize - w.off))
	w.putUint64(0) // Stream continuation possibility, should be 0.

	buf := bytes.NewBuffer(w.cur[0:w.off])
	n, err := io.Copy(w.blks, buf)
	if err != nil {
		return err
	}
	if int(n) != w.off {
		return errors.New("idxClose: r.cur short write")
	}
	return nil
}

// streamClose will flush the remainder of an single stream
func streamClose(w *writer) (err error) {
	// Insert length of remaining data into index
	w.putUint64(uint64(math.MaxUint64))
	w.putUint64(uint64(w.maxSize - w.off))

	buf := bytes.NewBuffer(w.cur[0:w.off])
	n, err := io.Copy(w.idx, buf)
	if err != nil {
		return err
	}
	if int(n) != w.off {
		return errors.New("streamClose: r.cur short write")
	}
	w.putUint64(0) // Stream continuation possibility, should be 0.
	return nil
}

// Close and flush the remaining data to output.
func (w *writer) Close() (err error) {
	select {
	case <-w.exited:
		return w.err
	default:
	}
	if w.flush != nil {
		err := w.flush(w)
		if err != nil {
			return err
		}
	}
	close(w.input)
	close(w.write)
	<-w.exited

	if w.close != nil {
		err := w.close(w)
		if err != nil {
			return err
		}
	}
	return w.err
}

// hasher will hash incoming blocks
// and signal the writer when done.
func (w *writer) hasher() {
	h := hasher.New()
	for b := range w.input {
		buf := bytes.NewBuffer(b.data)
		h.Reset()
		n, err := io.Copy(h, buf)
		if err != nil {
			w.setErr(err)
			return
		}
		if int(n) != len(b.data) {
			w.setErr(errors.New("short copy in hasher"))
			return
		}
		_ = h.Sum(b.sha1Hash[:0])
		b.hashDone <- nil
	}
}

// blockWriter will write hashed blocks to the output
// and recycle the buffers.
func (w *writer) blockWriter() {
	defer close(w.exited)

	sortA := make([]int, w.maxBlocks+1)

	for b := range w.write {
		_ = <-b.hashDone
		match, ok := w.index[b.sha1Hash]
		if !ok {
			buf := bytes.NewBuffer(b.data)
			n, err := io.Copy(w.blks, buf)
			if err != nil {
				w.setErr(err)
				return
			}
			if int(n) != len(b.data) {
				// This should not be possible with io.copy without an error,
				// but we test anyway.
				w.setErr(errors.New("error: short write on copy"))
				return
			}
			w.putUint64(0)
			w.putUint64(uint64(w.maxSize) - uint64(n))
		} else {
			offset := b.N - match
			if offset <= 0 {
				// should be impossible, indicated an internal error
				w.setErr(errors.New("internal error: negative offset"))
				return
			}
			w.putUint64(uint64(offset))
		}
		// Update hash to latest match
		w.index[b.sha1Hash] = b.N

		// Purge the entries with the oldest matches
		if w.maxBlocks > 0 && len(w.index) > w.maxBlocks {
			ar := sortA[0:len(w.index)]
			i := 0
			for _, v := range w.index {
				ar[i] = v
				i++
			}
			sort.Asc(ar)
			// Cut the oldest quarter blocks
			// since this isn't free
			cutoff := ar[w.maxBlocks/4]
			for k, v := range w.index {
				if v < cutoff {
					delete(w.index, k)
				}
			}
		}

		// Done, reinsert buffer
		w.buffers <- b
	}
}

// blockStreamWriter will write blocks and indexes to the output stream
// and recycle the buffers.
func (w *writer) blockStreamWriter() {
	defer close(w.exited)
	for b := range w.write {
		_ = <-b.hashDone
		match, ok := w.index[b.sha1Hash]
		if w.maxBlocks > 0 && (b.N-match) > w.maxBlocks {
			ok = false
		}
		if !ok {
			w.putUint64(0)
			w.putUint64(uint64(w.maxSize) - uint64(len(b.data)))
			buf := bytes.NewBuffer(b.data)
			n, err := io.Copy(w.idx, buf)
			if err != nil {
				w.setErr(err)
				return
			}
			if int(n) != len(b.data) {
				// This should not be possible with io.Copy without an error,
				// but we test anyway.
				w.setErr(errors.New("error: short write on copy"))
				return
			}
		} else {
			offset := b.N - match
			if offset <= 0 {
				// should be impossible, indicated an internal error
				w.setErr(errors.New("internal error: negative offset"))
				return
			}
			w.putUint64(uint64(offset))
		}
		// Update hash to latest match
		w.index[b.sha1Hash] = b.N

		// Purge old entries once in a while
		if w.maxBlocks > 0 && b.N&65535 == 65535 {
			for k, v := range w.index {
				if (b.N - v) > w.maxBlocks {
					delete(w.index, k)
				}
			}
		}
		// Done, reinsert buffer
		w.buffers <- b
	}
}

// fragmentWriter will write hashed blocks to the output channel
// and recycle the buffers.
func (w *writer) fragmentWriter() {
	defer close(w.exited)
	defer close(w.frags)
	n := uint(0)
	for b := range w.write {
		_ = <-b.hashDone
		var f Fragment
		f.N = n
		copy(f.Hash[:], b.sha1Hash[:])
		_, ok := w.index[b.sha1Hash]
		f.Payload = make([]byte, len(b.data))
		copy(f.Payload, b.data)
		if !ok {
			w.index[b.sha1Hash] = 0
			f.New = !ok
		}
		w.frags <- f
		// Done, reinsert buffer
		w.buffers <- b
		n++
	}
}

type fixedWriter struct{}

// Write blocks of similar size.
func (f *fixedWriter) write(w *writer, b []byte) (n int, err error) {
	written := 0
	for len(b) > 0 {
		n := copy(w.cur[w.off:], b)
		b = b[n:]
		w.off += n
		written += n
		// Filled the buffer? Send it off!
		if w.off == w.maxSize {
			b := <-w.buffers
			// Swap block with current
			w.cur, b.data = b.data, w.cur
			w.mu.Lock()
			b.N = w.nblocks
			w.nblocks++
			w.mu.Unlock()

			w.input <- b
			w.write <- b
			w.off = 0
		}
	}
	return written, nil
}

// Split content, so a new block begins with next write
func (f *fixedWriter) split(w *writer) {
	if w.off == 0 {
		return
	}
	b := <-w.buffers
	// Swap block with current
	w.cur, b.data = b.data[:w.maxSize], w.cur[:w.off]
	w.mu.Lock()
	b.N = w.nblocks
	w.nblocks++
	w.mu.Unlock()

	w.input <- b
	w.write <- b
	w.off = 0
}

// MemUse returns an approximate maximum memory use in bytes for
// encoder (Writer) and decoder (Reader) for the given number of bytes.
func (w *writer) MemUse(bytes int) (encoder, decoder int64) {
	blocks := (bytes + w.maxSize - 1) / w.maxSize
	if w.maxBlocks > 0 {
		if w.maxBlocks < blocks {
			blocks = w.maxBlocks
		}
	}
	// Data length
	data := big.NewInt(int64(blocks))
	data = data.Mul(data, big.NewInt(int64(w.maxSize)))
	d := data.Int64()
	if data.BitLen() > 63 {
		d = math.MaxInt64
	}
	// Index length
	bl := big.NewInt(int64(blocks))
	perBlock := big.NewInt(int64(HashSize + 8 /*int64*/ + 24 /* map entry*/))
	total := bl.Mul(bl, perBlock)
	if total.BitLen() > 63 {
		return math.MaxInt64, d
	}
	return total.Int64(), d
}

// Split blocks like ZPAQ: (public domain)
type zpaqWriter struct {
	h           uint32 // rolling hash for finding fragment boundaries
	c1          byte   // last byte
	maxFragment int
	minFragment int
	maxHash     uint32
	o1          [256]byte // order 1 context -> predicted byte
}

// Split blocks. Typically block size will be maxSize / 4
// Minimum block size is maxSize/64.
//
// The break point is content dependent.
// Any insertions, deletions, or edits that occur before the start of the 32+ byte dependency window
// don't affect the break point.
// This makes it likely for two files to still have identical fragments far away from any edits.
func newZpaqWriter(maxSize uint) *zpaqWriter {
	fragment := math.Log2(float64(maxSize) / (64 * 64))
	mh := math.Exp2(22 - fragment)
	return &zpaqWriter{
		maxFragment: int(maxSize),
		minFragment: int(maxSize / 64),
		maxHash:     uint32(mh),
	}
}

// h is a 32 bit hash that depends on the last 32 bytes that were mispredicted by the order 1 model o1[].
// h < maxhash therefore occurs with probability 2^-16, giving an average fragment size of 64K.
// The variable size dependency window works because one constant is odd (correct prediction, no shift),
// and the other is even but not a multiple of 4 (missed prediction, 1 bit shift left).
// This is different from a normal Rabin filter, which uses a large fixed-sized dependency window
// and two multiply operations, one at the window entry and the inverse at the window exit.
func (z *zpaqWriter) write(w *writer, b []byte) (int, error) {
	// Transfer to local variables ~30% faster.
	c1 := z.c1
	h := z.h
	off := w.off
	for _, c := range b {
		if c == z.o1[c1] {
			h = (h + uint32(c) + 1) * 314159265
		} else {
			h = (h + uint32(c) + 1) * 271828182
		}
		z.o1[c1] = c
		c1 = c
		w.cur[off] = c
		off++

		// At a break point? Send it off!
		if (off >= z.minFragment && h < z.maxHash) || off >= z.maxFragment {
			b := <-w.buffers
			// Swap block with current
			w.cur, b.data = b.data[:w.maxSize], w.cur[:off]
			b.N = w.nblocks

			w.input <- b
			w.write <- b
			w.nblocks++
			off = 0
			h = 0
			c1 = 0
		}
	}
	w.off = off
	z.h = h
	z.c1 = c1
	return len(b), nil
}

// Split content, so a new block begins with next write
func (z *zpaqWriter) split(w *writer) {
	if w.off == 0 {
		return
	}
	b := <-w.buffers
	// Swap block with current
	w.cur, b.data = b.data[:w.maxSize], w.cur[:w.off]
	w.mu.Lock()
	b.N = w.nblocks
	w.nblocks++
	w.mu.Unlock()

	w.input <- b
	w.write <- b
	w.off = 0
	z.h = 0
	z.c1 = 0
}

// Split blocks based on entropy distribution.
type entWriter struct {
	h           uint32 // rolling hash for finding fragment boundaries
	maxFragment int
	minFragment int
	maxHash     uint32
	hist        [256]uint16 // histogram of current accumulated
	histLen     int
	avgHist     uint16
}

// Split blocks. Typically block size will be maxSize / 4
// Minimum block size is maxSize/32.
//
// The break point is content dependent.
// Any insertions, deletions, or edits that occur before the start of the 32+ byte dependency window
// don't affect the break point.
// This makes it likely for two files to still have identical fragments far away from any edits.
func newEntropyWriter(maxSize uint) *entWriter {
	fragment := math.Log2(float64(maxSize) / (64 * 64))
	mh := math.Exp2(22 - fragment)
	e := &entWriter{
		maxFragment: int(maxSize),
		minFragment: int(maxSize / 32),
		maxHash:     uint32(mh),
	}
	if e.minFragment > 65535 {
		e.minFragment = 65535
	}
	if e.minFragment < 512 {
		e.minFragment = 512
	}
	e.avgHist = uint16(e.minFragment / 255)
	return e
}

// h is a 32 bit hash that depends on the last 32 bytes that were mispredicted by the order 1 model o1[].
// h < maxhash therefore occurs with probability 2^-16, giving an average fragment size of 64K.
// The variable size dependency window works because one constant is odd (correct prediction, no shift),
// and the other is even but not a multiple of 4 (missed prediction, 1 bit shift left).
// This is different from a normal Rabin filter, which uses a large fixed-sized dependency window
// and two multiply operations, one at the window entry and the inverse at the window exit.
func (e *entWriter) write(w *writer, b []byte) (int, error) {
	inLen := len(b)
	if e.histLen < e.minFragment {
		b2 := b
		if len(b2)+e.histLen > e.minFragment {
			b2 = b2[:e.minFragment-e.histLen]
		}
		off := w.off
		for i := range b2 {
			v := b2[i]
			e.hist[v]++
			w.cur[off+i] = v
		}
		e.histLen += len(b2)
		w.off += len(b2)
		b = b[len(b2):]
	}
	if len(b) == 0 {
		return inLen, nil
	}

	// Transfer to local variables ~30% faster.
	h := e.h
	off := w.off
	for _, c := range b {
		if e.hist[c] >= e.avgHist {
			h = (h + uint32(c) + 1) * 314159265
		} else {
			h = (h + uint32(c) + 1) * 271828182
		}
		w.cur[off] = c
		off++

		// At a break point? Send it off!
		if (off >= e.minFragment && h < e.maxHash) || off >= e.maxFragment {
			b := <-w.buffers
			// Swap block with current
			w.cur, b.data = b.data[:w.maxSize], w.cur[:off]
			b.N = w.nblocks

			w.input <- b
			w.write <- b
			e.histLen = 0
			for i := range e.hist {
				e.hist[i] = 0
			}
			w.nblocks++
			off = 0
			h = 0
		}
	}
	w.off = off
	e.h = h
	return inLen, nil
}

// Split content, so a new block begins with next write
func (e *entWriter) split(w *writer) {
	if w.off == 0 {
		return
	}
	b := <-w.buffers
	// Swap block with current
	w.cur, b.data = b.data[:w.maxSize], w.cur[:w.off]
	w.mu.Lock()
	b.N = w.nblocks
	w.nblocks++
	w.mu.Unlock()

	w.input <- b
	w.write <- b
	w.off = 0
	e.h = 0
	e.histLen = 0
	for i := range e.hist {
		e.hist[i] = 0
	}
}
