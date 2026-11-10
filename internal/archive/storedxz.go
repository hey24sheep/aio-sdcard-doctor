package archive

import (
	"bytes"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"hash/crc64"
)

// storedXZ writes one .xz stream whose data is kept as stored (uncompressed) LZMA2 chunks: a valid
// .xz any xz reader decodes, written at copy speed. Photos and videos don't compress, and the LZMA
// encoder crawls on them (~0.4 MB/s), so CompressImage stores such pieces instead.
// Format: tukaani.org/xz/xz-file-format.txt (stream, one block, index, footer; CRC64 check).
type storedXZ struct {
	buf        *bytes.Buffer
	crc        hash.Hash64
	size, comp int64 // uncompressed bytes; LZMA2 bytes written so far
}

var (
	xzFlags     = []byte{0x00, 0x04} // check type CRC64
	crc64Table  = crc64.MakeTable(crc64.ECMA)
	storedBlock = func() []byte {
		// header size byte ((12/4)-1), block flags (one filter, no sizes), LZMA2 filter (ID 0x21,
		// one property byte: dictionary 4 KiB — stored chunks use none), padding, then CRC32
		h := []byte{0x02, 0x00, 0x21, 0x01, 0x00, 0x00, 0x00, 0x00}
		return binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE(h))
	}()
)

const storedChunk = 64 << 10 // the largest LZMA2 uncompressed chunk

func newStoredXZ(buf *bytes.Buffer) *storedXZ {
	buf.Write([]byte{0xFD, '7', 'z', 'X', 'Z', 0x00})
	buf.Write(xzFlags)
	buf.Write(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(xzFlags)))
	buf.Write(storedBlock)
	return &storedXZ{buf: buf, crc: crc64.New(crc64Table)}
}

func (s *storedXZ) write(p []byte) {
	for len(p) > 0 {
		c := p[:min(len(p), storedChunk)]
		p = p[len(c):]
		ctl := byte(0x02) // uncompressed chunk
		if s.comp == 0 {
			ctl = 0x01 // the first one also resets the dictionary
		}
		s.buf.Write([]byte{ctl, byte((len(c) - 1) >> 8), byte(len(c) - 1)})
		s.buf.Write(c)
		s.crc.Write(c)
		s.size += int64(len(c))
		s.comp += 3 + int64(len(c))
	}
}

func (s *storedXZ) close() {
	s.buf.WriteByte(0x00) // LZMA2 end marker
	s.comp++
	for i := s.comp; i%4 != 0; i++ { // block padding
		s.buf.WriteByte(0)
	}
	s.buf.Write(binary.LittleEndian.AppendUint64(nil, s.crc.Sum64()))
	unpadded := int64(len(storedBlock)) + s.comp + 8
	idx := []byte{0x00}
	idx = binary.AppendUvarint(idx, 1)
	idx = binary.AppendUvarint(idx, uint64(unpadded))
	idx = binary.AppendUvarint(idx, uint64(s.size))
	for len(idx)%4 != 0 {
		idx = append(idx, 0)
	}
	idx = binary.LittleEndian.AppendUint32(idx, crc32.ChecksumIEEE(idx))
	s.buf.Write(idx)
	ft := binary.LittleEndian.AppendUint32(nil, uint32(len(idx)/4-1))
	ft = append(ft, xzFlags...)
	s.buf.Write(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(ft)))
	s.buf.Write(ft)
	s.buf.Write([]byte{'Y', 'Z'})
}
