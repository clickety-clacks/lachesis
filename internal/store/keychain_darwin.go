//go:build darwin && cgo

package store

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>

static CFStringRef tb_string(const char *s) {
  return CFStringCreateWithCString(NULL, s, kCFStringEncodingUTF8);
}

static OSStatus tb_read(const char *service, const char *account, UInt8 **out, CFIndex *n) {
  CFStringRef s=tb_string(service), a=tb_string(account);
  const void *keys[]={kSecClass,kSecAttrService,kSecAttrAccount,kSecReturnData,kSecMatchLimit};
  const void *vals[]={kSecClassGenericPassword,s,a,kCFBooleanTrue,kSecMatchLimitOne};
  CFDictionaryRef q=CFDictionaryCreate(NULL,keys,vals,5,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
  CFTypeRef result=NULL; OSStatus status=SecItemCopyMatching(q,&result);
  if(status==errSecSuccess){CFDataRef d=(CFDataRef)result; *n=CFDataGetLength(d); *out=malloc(*n); if(*n>0) CFDataGetBytes(d,CFRangeMake(0,*n),*out); CFRelease(result);}
  CFRelease(q); CFRelease(s); CFRelease(a); return status;
}

static OSStatus tb_update(const char *service,const char *account,const UInt8 *bytes,CFIndex n){
  CFStringRef s=tb_string(service),a=tb_string(account); CFDataRef d=CFDataCreate(NULL,bytes,n);
  const void *qk[]={kSecClass,kSecAttrService,kSecAttrAccount}; const void *qv[]={kSecClassGenericPassword,s,a};
  CFDictionaryRef q=CFDictionaryCreate(NULL,qk,qv,3,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
  const void *ak[]={kSecValueData}; const void *av[]={d};
  CFDictionaryRef attrs=CFDictionaryCreate(NULL,ak,av,1,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
  OSStatus status=SecItemUpdate(q,attrs); CFRelease(attrs);CFRelease(q);CFRelease(d);CFRelease(s);CFRelease(a);return status;
}

static OSStatus tb_add(const char *service,const char *account,const UInt8 *bytes,CFIndex n){
  CFStringRef s=tb_string(service),a=tb_string(account); CFDataRef d=CFDataCreate(NULL,bytes,n);
  const void *keys[]={kSecClass,kSecAttrService,kSecAttrAccount,kSecValueData}; const void *vals[]={kSecClassGenericPassword,s,a,d};
  CFDictionaryRef q=CFDictionaryCreate(NULL,keys,vals,4,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
  OSStatus status=SecItemAdd(q,NULL);CFRelease(q);CFRelease(d);CFRelease(s);CFRelease(a);return status;
}
*/
import "C"

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/clickety-clacks/lachesis/internal/model"
)

var ErrAtomicUnavailable = errors.New("native keychain atomic commit is unavailable")

type Keychain struct{ binding model.StoreBinding }

func NewKeychain(service, account string) (*Keychain, error) {
	if service == "" || account == "" {
		return nil, errors.New("keychain service and account are required")
	}
	return &Keychain{binding: model.StoreBinding{Kind: "keychain", Service: service, Account: account}}, nil
}
func (k *Keychain) Binding() model.StoreBinding { return k.binding }
func (k *Keychain) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	s := C.CString(k.binding.Service)
	a := C.CString(k.binding.Account)
	defer C.free(unsafe.Pointer(s))
	defer C.free(unsafe.Pointer(a))
	var out *C.UInt8
	var n C.CFIndex
	status := C.tb_read(s, a, &out, &n)
	if status == C.errSecItemNotFound {
		return nil, os.ErrNotExist
	}
	if status != 0 {
		return nil, fmt.Errorf("keychain read status %d", int(status))
	}
	defer C.free(unsafe.Pointer(out))
	return C.GoBytes(unsafe.Pointer(out), C.int(n)), nil
}
func (k *Keychain) Digest(ctx context.Context) ([32]byte, error) {
	b, err := k.Read(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}
func (k *Keychain) Commit(ctx context.Context, expected [32]byte, candidate []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	current, readErr := k.Read(ctx)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	exists := readErr == nil
	if exists && sha256.Sum256(current) != expected {
		return ErrChanged
	}
	s := C.CString(k.binding.Service)
	a := C.CString(k.binding.Account)
	defer C.free(unsafe.Pointer(s))
	defer C.free(unsafe.Pointer(a))
	var p *C.UInt8
	if len(candidate) > 0 {
		p = (*C.UInt8)(unsafe.Pointer(&candidate[0]))
	}
	var status C.OSStatus
	if exists {
		status = C.tb_update(s, a, p, C.CFIndex(len(candidate)))
	} else {
		status = C.tb_add(s, a, p, C.CFIndex(len(candidate)))
	}
	if status != 0 {
		if status == C.errSecDuplicateItem {
			return ErrChanged
		}
		return fmt.Errorf("keychain commit status %d", int(status))
	}
	return nil
}
