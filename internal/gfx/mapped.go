package gfx

import "unsafe"

// mapped views n bytes of memory returned by MapTransferBuffer. The memory is
// owned by the driver, not Go, so reinterpreting the address is safe until
// the buffer is unmapped.
func mapped(addr uintptr, n int) []byte {
	return unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))), n)
}
