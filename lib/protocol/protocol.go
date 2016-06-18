// Copyright (C) 2014 The Protocol Authors.

package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	lz4 "github.com/bkaradzic/go-lz4"
)

const (
	// BlockSize is the standard ata block size (128 KiB)
	BlockSize = 128 << 10

	// MaxMessageLen is the largest message size allowed on the wire. (512 MiB)
	MaxMessageLen = 64 << 23
)

const (
	stateInitial = iota
	stateReady
)

// Request message flags
const (
	FlagFromTemporary uint32 = 1 << iota
)

// ClusterConfigMessage.Folders flags
const (
	FlagFolderReadOnly            uint32 = 1 << 0
	FlagFolderIgnorePerms                = 1 << 1
	FlagFolderIgnoreDelete               = 1 << 2
	FlagFolderDisabledTempIndexes        = 1 << 3
	FlagFolderAll                        = 1<<4 - 1
)

// ClusterConfigMessage.Folders.Devices flags
const (
	FlagShareTrusted  uint32 = 1 << 0
	FlagShareReadOnly        = 1 << 1
	FlagIntroducer           = 1 << 2
	FlagShareBits            = 0x000000ff
)

var (
	ErrClosed               = errors.New("connection closed")
	ErrTimeout              = errors.New("read timeout")
	ErrSwitchingConnections = errors.New("switching connections")
)

type Model interface {
	// An index was received from the peer device
	Index(deviceID DeviceID, folder string, files []FileInfo)
	// An index update was received from the peer device
	IndexUpdate(deviceID DeviceID, folder string, files []FileInfo)
	// A request was made by the peer device
	Request(deviceID DeviceID, folder string, name string, offset int64, hash []byte, fromTemporary bool, buf []byte) error
	// A cluster configuration message was received
	ClusterConfig(deviceID DeviceID, config ClusterConfig)
	// The peer device closed the connection
	Close(deviceID DeviceID, err error)
	// The peer device sent progress updates for the files it is currently downloading
	DownloadProgress(deviceID DeviceID, folder string, updates []FileDownloadProgressUpdate)
}

type Connection interface {
	Start()
	ID() DeviceID
	Name() string
	Index(folder string, files []FileInfo) error
	IndexUpdate(folder string, files []FileInfo) error
	Request(folder string, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error)
	ClusterConfig(config ClusterConfig)
	DownloadProgress(folder string, updates []FileDownloadProgressUpdate)
	Statistics() Statistics
	Closed() bool
}

type rawConnection struct {
	id       DeviceID
	name     string
	receiver Model

	cr *countingReader
	cw *countingWriter

	awaiting    map[int32]chan asyncResult
	awaitingMut sync.Mutex

	idxMut sync.Mutex // ensures serialization of Index calls

	nextID    int32
	nextIDMut sync.Mutex

	outbox      chan hdrMsg
	closed      chan struct{}
	once        sync.Once
	pool        sync.Pool
	compression Compression

	// used by readMessage
	readerBuf []byte

	// used by the lz4 methods
	lz4DecompBuf []byte
	lz4CompBuf   []byte
	lz4UncompBuf []byte

	// used by compressMessage and writeMessage
	writeBuf []byte
}

type asyncResult struct {
	val []byte
	err error
}

type hdrMsg struct {
	msg  Message
	done chan struct{}
}

const (
	// PingSendInterval is how often we make sure to send a message, by
	// triggering pings if necessary.
	PingSendInterval = 90 * time.Second
	// ReceiveTimeout is the longest we'll wait for a message from the other
	// side before closing the connection.
	ReceiveTimeout = 300 * time.Second
)

func NewConnection(deviceID DeviceID, reader io.Reader, writer io.Writer, receiver Model, name string, compress Compression) Connection {
	cr := &countingReader{Reader: reader}
	cw := &countingWriter{Writer: writer}

	c := rawConnection{
		id:       deviceID,
		name:     name,
		receiver: nativeModel{receiver},
		cr:       cr,
		cw:       cw,
		awaiting: make(map[int32]chan asyncResult),
		outbox:   make(chan hdrMsg),
		closed:   make(chan struct{}),
		pool: sync.Pool{
			New: func() interface{} {
				return make([]byte, BlockSize)
			},
		},
		compression: compress,
	}

	return wireFormatConnection{&c}
}

// Start creates the goroutines for sending and receiving of messages. It must
// be called exactly once after creating a connection.
func (c *rawConnection) Start() {
	go c.readerLoop()
	go c.writerLoop()
	go c.pingSender()
	go c.pingReceiver()
}

func (c *rawConnection) ID() DeviceID {
	return c.id
}

func (c *rawConnection) Name() string {
	return c.name
}

// Index writes the list of file information to the connected peer device
func (c *rawConnection) Index(folder string, idx []FileInfo) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.idxMut.Lock()
	c.send(Message{Index: &Index{
		Folder: folder,
		Files:  idx,
	}}, nil)
	c.idxMut.Unlock()
	return nil
}

// IndexUpdate writes the list of file information to the connected peer device as an update
func (c *rawConnection) IndexUpdate(folder string, idx []FileInfo) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.idxMut.Lock()
	c.send(Message{IndexUpdate: &Index{
		Folder: folder,
		Files:  idx,
	}}, nil)
	c.idxMut.Unlock()
	return nil
}

// Request returns the bytes for the specified block after fetching them from the connected peer.
func (c *rawConnection) Request(folder string, name string, offset int64, size int, hash []byte, fromTemporary bool) ([]byte, error) {
	c.nextIDMut.Lock()
	id := c.nextID
	c.nextID++
	c.nextIDMut.Unlock()

	c.awaitingMut.Lock()
	if _, ok := c.awaiting[id]; ok {
		panic("id taken")
	}
	rc := make(chan asyncResult, 1)
	c.awaiting[id] = rc
	c.awaitingMut.Unlock()

	ok := c.send(Message{Request: &Request{
		ID:            id,
		Folder:        folder,
		Name:          name,
		Offset:        offset,
		Size:          int32(size),
		Hash:          hash,
		FromTemporary: fromTemporary,
	}}, nil)
	if !ok {
		return nil, ErrClosed
	}

	res, ok := <-rc
	if !ok {
		return nil, ErrClosed
	}
	return res.val, res.err
}

// ClusterConfig send the cluster configuration message to the peer and returns any error
func (c *rawConnection) ClusterConfig(config ClusterConfig) {
	c.send(Message{ClusterConfig: &config}, nil)
}

func (c *rawConnection) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// DownloadProgress sends the progress updates for the files that are currently being downloaded.
func (c *rawConnection) DownloadProgress(folder string, updates []FileDownloadProgressUpdate) {
	c.send(Message{DownloadProgress: &DownloadProgress{
		Folder:  folder,
		Updates: updates,
	}}, nil)
}

func (c *rawConnection) ping() bool {
	return c.send(Message{Ping: &Ping{}}, nil)
}

func (c *rawConnection) readerLoop() (err error) {
	defer func() {
		c.close(err)
	}()

	state := stateInitial
	for {
		select {
		case <-c.closed:
			return ErrClosed
		default:
		}

		msg, err := c.readMessage()
		if err != nil {
			return err
		}

		switch {
		case msg.ClusterConfig != nil:
			l.Debugln("read ClusterConfig message")
			if state != stateInitial {
				return fmt.Errorf("protocol error: cluster config message in state %d", state)
			}
			go c.receiver.ClusterConfig(c.id, *msg.ClusterConfig)
			state = stateReady

		case msg.Index != nil:
			l.Debugln("read Index message")
			if state != stateReady {
				return fmt.Errorf("protocol error: index message in state %d", state)
			}
			c.handleIndex(*msg.Index)
			state = stateReady

		case msg.IndexUpdate != nil:
			l.Debugln("read IndexUpdate message")
			if state != stateReady {
				return fmt.Errorf("protocol error: index update message in state %d", state)
			}
			c.handleIndexUpdate(*msg.IndexUpdate)
			state = stateReady

		case msg.Request != nil:
			l.Debugln("read Request message")
			if state != stateReady {
				return fmt.Errorf("protocol error: request message in state %d", state)
			}
			// Requests are handled asynchronously
			go c.handleRequest(*msg.Request)

		case msg.Response != nil:
			l.Debugln("read Response message")
			if state != stateReady {
				return fmt.Errorf("protocol error: response message in state %d", state)
			}
			c.handleResponse(*msg.Response)

		case msg.DownloadProgress != nil:
			l.Debugln("read DownloadProgress message")
			if state != stateReady {
				return fmt.Errorf("protocol error: response message in state %d", state)
			}
			c.receiver.DownloadProgress(c.id, msg.DownloadProgress.Folder, msg.DownloadProgress.Updates)

		case msg.Ping != nil:
			l.Debugln("read Ping message")
			if state != stateReady {
				return fmt.Errorf("protocol error: ping message in state %d", state)
			}
			// Nothing

		case msg.Close != nil:
			l.Debugln("read Close message")
			return errors.New(msg.Close.Reason)

		default:
			l.Debugf("read unknown message: %+v", msg)
			return fmt.Errorf("protocol error: %s: unknown or empty message", c.id)
		}
	}
}

func (c *rawConnection) readMessage() (Message, error) {
	// First comes a 32 bit length-of-message word

	if len(c.readerBuf) < 4 {
		c.readerBuf = make([]byte, 4)
	}

	if _, err := io.ReadFull(c.cr, c.readerBuf[:4]); err != nil {
		return Message{}, fmt.Errorf("reading length word: %v", err)
	}

	msgLen := binary.BigEndian.Uint32(c.readerBuf)

	// Then the actual message

	if len(c.readerBuf) < int(msgLen) {
		c.readerBuf = make([]byte, msgLen)
	}

	if _, err := io.ReadFull(c.cr, c.readerBuf[:msgLen]); err != nil {
		return Message{}, fmt.Errorf("reading message: %v", err)
	}

	var msg Message
	if err := msg.Unmarshal(c.readerBuf[:msgLen]); err != nil {
		return Message{}, fmt.Errorf("unmarshalling message: %v", err)
	}

	if msg.Compressed != nil {
		buf, err := c.lz4Decompress(msg.Compressed.Data, msg.Compressed.UncompressedLength)
		if err != nil {
			return Message{}, fmt.Errorf("decompressing message (%d -> %d): %v", len(msg.Compressed.Data), msg.Compressed.UncompressedLength, err)
		}

		if err := msg.Unmarshal(buf); err != nil {
			return Message{}, fmt.Errorf("unmarshalling compressed message: %v", err)
		}
	}

	return msg, nil
}

func (c *rawConnection) handleIndex(im Index) {
	l.Debugf("Index(%v, %v, %d file)", c.id, im.Folder, len(im.Files))
	c.receiver.Index(c.id, im.Folder, filterIndexMessageFiles(im.Files))
}

func (c *rawConnection) handleIndexUpdate(im Index) {
	l.Debugf("queueing IndexUpdate(%v, %v, %d files)", c.id, im.Folder, len(im.Files))
	c.receiver.IndexUpdate(c.id, im.Folder, filterIndexMessageFiles(im.Files))
}

func filterIndexMessageFiles(fs []FileInfo) []FileInfo {
	var out []FileInfo
	for i, f := range fs {
		switch f.Name {
		case "", ".", "..", "/": // A few obviously invalid filenames
			l.Infof("Dropping invalid filename %q from incoming index", f.Name)
			if out == nil {
				// Most incoming updates won't contain anything invalid, so we
				// delay the allocation and copy to output slice until we
				// really need to do it, then copy all the so var valid files
				// to it.
				out = make([]FileInfo, i, len(fs)-1)
				copy(out, fs)
			}
		default:
			if out != nil {
				out = append(out, f)
			}
		}
	}
	if out != nil {
		return out
	}
	return fs
}

func (c *rawConnection) handleRequest(req Request) {
	size := int(req.Size)
	usePool := size <= BlockSize

	var buf []byte
	var done chan struct{}

	if usePool {
		buf = c.pool.Get().([]byte)[:size]
		done = make(chan struct{})
	} else {
		buf = make([]byte, size)
	}

	err := c.receiver.Request(c.id, req.Folder, req.Name, int64(req.Offset), req.Hash, req.FromTemporary, buf)
	if err != nil {
		c.send(Message{Response: &Response{
			ID:   req.ID,
			Data: nil,
			Code: errorToCode(err),
		}}, done)
	} else {
		c.send(Message{Response: &Response{
			ID:   req.ID,
			Data: buf,
			Code: errorToCode(err),
		}}, done)
	}

	if usePool {
		<-done
		c.pool.Put(buf)
	}
}

func (c *rawConnection) handleResponse(resp Response) {
	c.awaitingMut.Lock()
	if rc := c.awaiting[resp.ID]; rc != nil {
		delete(c.awaiting, resp.ID)
		rc <- asyncResult{resp.Data, codeToError(resp.Code)}
		close(rc)
	}
	c.awaitingMut.Unlock()
}

func (c *rawConnection) send(msg Message, done chan struct{}) bool {
	select {
	case c.outbox <- hdrMsg{msg, done}:
		return true
	case <-c.closed:
		return false
	}
}

func (c *rawConnection) writerLoop() {
	for {
		select {
		case hm := <-c.outbox:
			msg, err := c.maybeCompressMessage(hm.msg)
			if err != nil {
				c.close(err)
				return
			}
			err = c.writeMessage(msg)
			if err != nil {
				c.close(err)
				return
			}
			if hm.done != nil {
				close(hm.done)
			}

		case <-c.closed:
			return
		}
	}
}

func (c *rawConnection) maybeCompressMessage(msg Message) (Message, error) {
	if msg.Compressed != nil {
		panic("bug: cannot compress compressed message")
	}

	if c.compression == CompressNever {
		return msg, nil
	}

	// Decide if we're going to use compression or not
	rawMsgLen := msg.ProtoSize()
	compress := false
	switch c.compression {
	case CompressAlways:
		// Use compression for large enough messages
		compress = rawMsgLen >= compressionThreshold
	case CompressMetadata:
		// Compress if it's large enough and not a response message
		compress = rawMsgLen >= compressionThreshold && msg.Response == nil
	}

	if !compress {
		return msg, nil
	}

	if len(c.writeBuf) < rawMsgLen {
		c.writeBuf = make([]byte, rawMsgLen)
	}
	if _, err := msg.MarshalTo(c.writeBuf); err != nil {
		return Message{}, err
	}

	buf, err := c.lz4Compress(c.writeBuf[:rawMsgLen])
	if err != nil {
		return Message{}, err
	}
	msg = Message{
		Compressed: &CompressedMessage{
			Data:               buf,
			UncompressedLength: int32(rawMsgLen),
		},
	}
	return msg, nil
}

func (c *rawConnection) writeMessage(msg Message) error {
	// Marshal the message into msgBuf
	rawMsgLen := msg.ProtoSize()
	if len(c.writeBuf) < 4+rawMsgLen {
		c.writeBuf = make([]byte, 4+rawMsgLen)
	}
	binary.BigEndian.PutUint32(c.writeBuf, uint32(rawMsgLen))
	if _, err := msg.MarshalTo(c.writeBuf[4:]); err != nil {
		return err
	}

	// Throw it on the wire
	n, err := c.cw.Write(c.writeBuf[:4+rawMsgLen])
	l.Debugf("wrote %d bytes on the wire (4 bytes length, %d bytes message), err=%v", n, rawMsgLen, err)
	return err
}

func (c *rawConnection) close(err error) {
	c.once.Do(func() {
		l.Debugln("close due to", err)
		close(c.closed)

		c.awaitingMut.Lock()
		for i, ch := range c.awaiting {
			if ch != nil {
				close(ch)
				delete(c.awaiting, i)
			}
		}
		c.awaitingMut.Unlock()

		go c.receiver.Close(c.id, err)
	})
}

// The pingSender makes sure that we've sent a message within the last
// PingSendInterval. If we already have something sent in the last
// PingSendInterval/2, we do nothing. Otherwise we send a ping message. This
// results in an effecting ping interval of somewhere between
// PingSendInterval/2 and PingSendInterval.
func (c *rawConnection) pingSender() {
	ticker := time.Tick(PingSendInterval / 2)

	for {
		select {
		case <-ticker:
			d := time.Since(c.cw.Last())
			if d < PingSendInterval/2 {
				l.Debugln(c.id, "ping skipped after wr", d)
				continue
			}

			l.Debugln(c.id, "ping -> after", d)
			c.ping()

		case <-c.closed:
			return
		}
	}
}

// The pingReceiver checks that we've received a message (any message will do,
// but we expect pings in the absence of other messages) within the last
// ReceiveTimeout. If not, we close the connection with an ErrTimeout.
func (c *rawConnection) pingReceiver() {
	ticker := time.Tick(ReceiveTimeout / 2)

	for {
		select {
		case <-ticker:
			d := time.Since(c.cr.Last())
			if d > ReceiveTimeout {
				l.Debugln(c.id, "ping timeout", d)
				c.close(ErrTimeout)
			}

			l.Debugln(c.id, "last read within", d)

		case <-c.closed:
			return
		}
	}
}

type Statistics struct {
	At            time.Time
	InBytesTotal  int64
	OutBytesTotal int64
}

func (c *rawConnection) Statistics() Statistics {
	return Statistics{
		At:            time.Now(),
		InBytesTotal:  c.cr.Tot(),
		OutBytesTotal: c.cw.Tot(),
	}
}

// The LZ4 package that we use is special in that it prefixes the compressed
// data with an uint32 containing the uncompressed length (and in little
// endian byte order no less). It expects that field to be there on
// decompression as well. We don't want that because it's nonstandard so we
// jump through some hoops here to get rid of it. At some point we may want
// to make a pull request to tweak the API as the LZ4 package could equally
// well provide the API we need as well.

func (c *rawConnection) lz4Compress(src []byte) ([]byte, error) {
	var err error
	c.lz4CompBuf, err = lz4.Encode(c.lz4CompBuf, src)
	if err != nil {
		return nil, err
	}
	return c.lz4CompBuf[4:], nil
}

func (c *rawConnection) lz4Decompress(src []byte, uncompressedSize int32) ([]byte, error) {
	if len(c.lz4DecompBuf) < 4+len(src) {
		c.lz4DecompBuf = make([]byte, 4+len(src))
	}
	// LittleEndian because ... I really have no idea.
	binary.LittleEndian.PutUint32(c.lz4DecompBuf, uint32(uncompressedSize))
	copy(c.lz4DecompBuf[4:], src)
	var err error
	c.lz4UncompBuf, err = lz4.Decode(c.lz4UncompBuf, c.lz4DecompBuf[:4+len(src)])
	if err != nil {
		return nil, err
	}
	return c.lz4UncompBuf, nil
}
