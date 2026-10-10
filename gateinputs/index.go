package gateinputs

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// NormalizeIndex is a git index (.git/index, versions 2 and 3) as `git read-tree` would write it for the same
// entries: every entry's stat data (ctime, mtime, dev, ino, uid, gid and size) zeroed and its mode, object, flags and
// path kept, and every optional extension (the cache tree, the untracked cache, the fsmonitor's and the offset
// tables, all of them derived) dropped, with the trailing checksum taken again. A zero size makes git compare an
// entry's content to the file rather than trust its stat (read-cache.c, ce_modified), so a checkout unpacked from it
// reads as it did where it was packed, and the index's bytes say nothing about when or where its files were written,
// or whether `git status` has refreshed it since. An index git doesn't need a reader to understand whole is refused:
// version 4's compressed paths, a split index (link) and a sparse index (sdir).
func NormalizeIndex(content []byte) ([]byte, error) {
	if len(content) < 12 || string(content[:4]) != "DIRC" {
		return nil, fmt.Errorf("not a git index")
	}
	hashSize := 0
	for _, size := range []int{sha1.Size, sha256.Size} {
		if len(content) < 12+size {
			continue
		}
		body, trailer := content[:len(content)-size], content[len(content)-size:]
		if bytes.Equal(checksum(body, size), trailer) {
			hashSize = size
			break
		}
	}
	if hashSize == 0 {
		return nil, fmt.Errorf("the index's checksum doesn't match it")
	}
	version := binary.BigEndian.Uint32(content[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("index version %d: only versions 2 and 3 are read", version)
	}
	count := binary.BigEndian.Uint32(content[8:12])
	body := content[:len(content)-hashSize]
	normalized := append([]byte{}, body[:12]...)
	offset := 12
	// statSize is ctime, mtime (each seconds and nanoseconds), dev, ino, mode, uid, gid and size, four bytes each.
	const statSize = 40
	for entry := uint32(0); entry < count; entry++ {
		fixed := statSize + hashSize + 2
		if offset+fixed > len(body) {
			return nil, fmt.Errorf("entry %d runs past the index", entry)
		}
		flags := binary.BigEndian.Uint16(body[offset+statSize+hashSize:])
		if flags&0x4000 != 0 {
			if version < 3 {
				return nil, fmt.Errorf("entry %d has extended flags in a version 2 index", entry)
			}
			fixed += 2
		}
		end := bytes.IndexByte(body[offset+fixed:], 0)
		if end < 0 {
			return nil, fmt.Errorf("entry %d's path has no end", entry)
		}
		size := (fixed + end + 8) &^ 7
		if offset+size > len(body) {
			return nil, fmt.Errorf("entry %d runs past the index", entry)
		}
		for _, padding := range body[offset+fixed+end : offset+size] {
			if padding != 0 {
				return nil, fmt.Errorf("entry %d's padding isn't zero", entry)
			}
		}
		record := append([]byte{}, body[offset:offset+size]...)
		clear(record[0:24])
		clear(record[28:statSize])
		normalized = append(normalized, record...)
		offset += size
	}
	for offset < len(body) {
		if offset+8 > len(body) {
			return nil, fmt.Errorf("an extension's header runs past the index")
		}
		signature := body[offset : offset+4]
		size := int(binary.BigEndian.Uint32(body[offset+4 : offset+8]))
		if size < 0 || offset+8+size > len(body) {
			return nil, fmt.Errorf("extension %q runs past the index", signature)
		}
		if signature[0] < 'A' || signature[0] > 'Z' {
			return nil, fmt.Errorf("extension %q is one a reader must understand (a split or sparse index), and only full indexes are read", signature)
		}
		offset += 8 + size
	}
	return append(normalized, checksum(normalized, hashSize)...), nil
}

func checksum(content []byte, size int) []byte {
	if size == sha1.Size {
		sum := sha1.Sum(content)
		return sum[:]
	}
	sum := sha256.Sum256(content)
	return sum[:]
}
