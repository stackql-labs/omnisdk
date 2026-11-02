package secretstore

import (
	"context"
	"encoding/base64"
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var advapi = syscall.NewLazyDLL("advapi32.dll")
var credRead = advapi.NewProc("CredReadW")
var credWrite = advapi.NewProc("CredWriteW")
var credDelete = advapi.NewProc("CredDeleteW")
var credFree = advapi.NewProc("CredFree")

type windowsCredential struct {
	Flags, Type             uint32
	TargetName, Comment     *uint16
	LastWritten             syscall.Filetime
	BlobSize                uint32
	Blob                    *byte
	Persist, AttributeCount uint32
	Attributes              uintptr
	TargetAlias, UserName   *uint16
}
type windowsStore struct{ prefix string }

func NewOS(namespace string) (facade.SecretStore, error) {
	for _, proc := range []*syscall.LazyProc{credRead, credWrite, credDelete, credFree} {
		if err := proc.Find(); err != nil {
			return nil, ErrUnavailable
		}
	}
	return windowsStore{prefix: "omnisdk/" + base64.RawURLEncoding.EncodeToString([]byte(namespace)) + "/"}, nil
}
func (s windowsStore) target(key string) (*uint16, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	name := s.prefix + base64.RawURLEncoding.EncodeToString([]byte(key))
	if len(name) > 32767 {
		return nil, errors.New("secret name too long")
	}
	return syscall.UTF16PtrFromString(name)
}
func windowsError(err error) error {
	if errors.Is(err, syscall.Errno(1168)) {
		return ErrNotFound
	}
	return ErrUnavailable
}
func (s windowsStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := s.target(key)
	if err != nil {
		return nil, err
	}
	var credential *windowsCredential
	ok, _, callErr := credRead.Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&credential)))
	if ok == 0 {
		return nil, windowsError(callErr)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(credential)))
	return append([]byte{}, unsafe.Slice(credential.Blob, int(credential.BlobSize))...), nil
}
func (s windowsStore) Set(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := s.target(key)
	if err != nil {
		return err
	}
	if len(value) > 2560 {
		return errors.New("Windows secret exceeds 2560 bytes")
	}
	credential := windowsCredential{Type: 1, TargetName: target, Persist: 2, BlobSize: uint32(len(value))}
	if len(value) > 0 {
		credential.Blob = &value[0]
	}
	ok, _, callErr := credWrite.Call(uintptr(unsafe.Pointer(&credential)), 0)
	runtime.KeepAlive(value)
	if ok == 0 {
		return windowsError(callErr)
	}
	return nil
}
func (s windowsStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := s.target(key)
	if err != nil {
		return err
	}
	ok, _, callErr := credDelete.Call(uintptr(unsafe.Pointer(target)), 1, 0)
	if ok == 0 {
		return windowsError(callErr)
	}
	return nil
}
