package configmirror

import (
	"encoding/binary"
	"errors"
	"io"
)

// Mirror writers emit single-disk, non-ZIP64 archives without archive comments.
// Validate that bounded subset before archive/zip allocates central-directory
// records. Checking len(Reader.File) afterwards is too late for hostile input.
func admitArchive(file io.ReaderAt, size int64) error {
	var end [22]byte
	if size < int64(len(end)) {
		return errors.New("config mirror archive is truncated")
	}
	if _, err := file.ReadAt(end[:], size-int64(len(end))); err != nil {
		return err
	}
	u16 := binary.LittleEndian.Uint16
	u32 := binary.LittleEndian.Uint32
	count := int(u16(end[10:12]))
	directorySize, offset := int64(u32(end[12:16])), int64(u32(end[16:20]))
	if u32(end[:4]) != 0x06054b50 || u16(end[4:6]) != 0 || u16(end[6:8]) != 0 || int(u16(end[8:10])) != count || u16(end[20:22]) != 0 {
		return errors.New("unsupported config mirror archive footer")
	}
	if count < 1 || count > MaxFiles || directorySize > 64<<20 || offset+directorySize != size-22 {
		return errors.New("config mirror directory exceeds admission limits")
	}
	directory := io.NewSectionReader(file, offset, directorySize)
	for i := 0; i < count; i++ {
		if err := admitDirectoryRecord(directory, offset); err != nil {
			return err
		}
	}
	position, err := directory.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if position != directorySize {
		return errors.New("config mirror directory count does not match its contents")
	}
	return nil
}

func admitDirectoryRecord(directory io.Reader, dataEnd int64) error {
	var header [46]byte
	if _, err := io.ReadFull(directory, header[:]); err != nil {
		return err
	}
	u16 := binary.LittleEndian.Uint16
	u32 := binary.LittleEndian.Uint32
	name, extra, comment := int64(u16(header[28:30])), int64(u16(header[30:32])), int64(u16(header[32:34]))
	if u32(header[:4]) != 0x02014b50 || u16(header[34:36]) != 0 || u16(header[8:10])&1 != 0 || u16(header[10:12]) != 0 {
		return errors.New("unsupported config mirror directory record")
	}
	if name < 1 || name > 4096 || extra > 4096 || comment > 4096 || u32(header[24:28]) > MaxFileBytes || int64(u32(header[42:46])) >= dataEnd {
		return errors.New("config mirror entry exceeds admission limits")
	}
	_, err := io.CopyN(io.Discard, directory, name+extra+comment)
	return err
}
