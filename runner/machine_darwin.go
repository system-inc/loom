package runner

import (
	"encoding/binary"
	"syscall"
)

func cgroupCpus() int { return 0 }

func cgroupMemoryBytes() uint64 { return 0 }

// physicalMemoryBytes reads hw.memsize. syscall.Sysctl hands back the uint64 as a string with its trailing
// zero bytes trimmed, so they are put back before decoding.
func physicalMemoryBytes() uint64 {
	value, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	raw := make([]byte, 8)
	copy(raw, value)
	return binary.LittleEndian.Uint64(raw)
}
