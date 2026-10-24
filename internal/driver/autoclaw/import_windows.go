//go:build windows

package autoclaw

import (
	"errors"
	"syscall"
	"unsafe"
)

// dpapiUnprotect unwraps a DPAPI blob with CryptUnprotectData.
//
// The blob is bound to the Windows user who created it, so this only succeeds
// while running as that user — which is exactly the property that makes the
// import safe: another account on the same machine cannot read these tokens.
//
// DATA_BLOB is {DWORD cbData; BYTE *pbData} and is laid out with 4 bytes of
// padding after cbData on 64-bit, hence the explicit Pad field. Getting this
// wrong produces a garbage pointer rather than a compile error, which is why
// the struct mirrors the Win32 header exactly.
type dataBlob struct {
	cbData uint32
	_      uint32
	pbData *byte
}

var (
	crypt32           = syscall.NewLazyDLL("crypt32.dll")
	procUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree     = kernel32.NewProc("LocalFree")
)

func dpapiUnprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("dpapi: empty input")
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	ret, _, err := procUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0, // dwFlags
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		if err != nil && err != syscall.Errno(0) {
			return nil, err
		}
		return nil, errors.New("dpapi: CryptUnprotectData failed")
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	if out.pbData == nil || out.cbData == 0 {
		return nil, errors.New("dpapi: empty result")
	}
	buf := unsafe.Slice(out.pbData, out.cbData)
	cp := make([]byte, len(buf))
	copy(cp, buf)
	return cp, nil
}
