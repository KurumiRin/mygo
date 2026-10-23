//go:build windows && (amd64 || arm64)

package windows

import (
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"github.com/egoist/mygo/internal/platform"
)

// winrtIID derives the IID of a parameterized WinRT interface from its
// signature, as the Windows Runtime does: a version 5 UUID in WinRT's
// namespace.
func winrtIID(signature string) GUID {
	ns := [16]byte{0x11, 0xf4, 0x7a, 0xd5, 0x7b, 0x73, 0x42, 0xc0, 0xab, 0xae, 0x87, 0x8b, 0x1e, 0x16, 0xad, 0xee}
	sum := sha1.Sum(append(ns[:], signature...))
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	var g GUID
	g.Data1 = binary.BigEndian.Uint32(sum[0:])
	g.Data2 = binary.BigEndian.Uint16(sum[4:])
	g.Data3 = binary.BigEndian.Uint16(sum[6:])
	copy(g.Data4[:], sum[8:16])
	return g
}

func TestWinRTIIDs(t *testing.T) {
	loadWinRT()
	const iterable, iterator, storageItem = "{faa585ea-6214-4217-afda-7f46de5869b3}", "{6a79e863-4300-459a-9966-cbb660963ee1}", "{4207a996-ca2f-42f7-bde8-8b10457a7f30}"
	for _, c := range []struct {
		signature string
		want      GUID
	}{
		// IIterable<String>, which the SDK's headers name, checks the
		// derivation.
		{"pinterface(" + iterable + ";string)", guid("e2fcc7c1-3bfc-5a0b-b2b0-72e769d1cb7e")},
		{"pinterface(" + iterable + ";" + storageItem + ")", winrt.iterable},
		{"pinterface(" + iterator + ";" + storageItem + ")", winrt.iterator},
	} {
		if got := winrtIID(c.signature); got != c.want {
			t.Errorf("IID of %s = %v, want %v", c.signature, got, c.want)
		}
	}
}

// hstring reads an HSTRING.
func hstring(h uintptr) string {
	var n uint32
	p, _, _ := winrt.stringBuffer.Call(h, uintptr(unsafe.Pointer(&n)))
	if p == 0 || n == 0 {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice((*uint16)(native(p)), n))
}

// longPath returns the long form of a path, which storage items give for
// the temporary directory's short one (RUNNER~1).
func longPath(t *testing.T, p string) string {
	t.Helper()
	buf := make([]uint16, syscall.MAX_PATH)
	n, err := syscall.GetLongPathName(u16(p), &buf[0], uint32(len(buf)))
	if err != nil || int(n) > len(buf) {
		t.Fatalf("GetLongPathName(%q): %v", p, err)
	}
	return syscall.UTF16ToString(buf[:n])
}

// inMTA runs the test on a thread of the MTA, as storageItems runs.
func inMTA(t *testing.T) {
	runtime.LockOSThread()
	if r, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded); !failed(r) {
		t.Cleanup(func() { procCoUninitialize.Call() })
	}
	t.Cleanup(runtime.UnlockOSThread)
}

// TestShareData makes the storage items of a file and a folder, and fills
// a data package as DataRequested does.
func TestShareData(t *testing.T) {
	inMTA(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := storageItems([]string{file, dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(storage) != 2 {
		t.Fatalf("storageItems made %d items, want 2", len(storage))
	}
	// IStorageItem::get_Path
	for i, want := range []string{file, dir} {
		var h uintptr
		if hr := comCall(storage[i], 12, uintptr(unsafe.Pointer(&h))); failed(hr) {
			t.Fatal(hresultError("IStorageItem.Path", hr))
		}
		if got := hstring(h); got != longPath(t, want) {
			t.Errorf("storage item %d is %q, want %q", i, got, want)
		}
		winrt.deleteString.Call(h)
	}
	if _, err := storageItems([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("storageItems of a missing file did not fail")
	}

	s := &sharing{items: &platform.ShareItems{Title: "Note", Text: "Look", URL: "https://example.com/a", Files: []string{file, dir}}, storage: storage}
	defer s.release()
	var obj uintptr
	hr := withHString("Windows.ApplicationModel.DataTransfer.DataPackage", func(h uintptr) uintptr {
		r, _, _ := winrt.activateInstance.Call(h, uintptr(unsafe.Pointer(&obj)))
		return r
	})
	if failed(hr) {
		t.Fatal(hresultError("RoActivateInstance(DataPackage)", hr))
	}
	defer release(obj)
	iidIDataPackage := guid("61ebf5c7-efea-4346-9554-981d7e198ffe")
	data := queryInterface(obj, &iidIDataPackage)
	if data == 0 {
		t.Fatal("the DataPackage has no IDataPackage")
	}
	defer release(data)
	if err := fillDataPackage(data, s); err != nil {
		t.Fatal(err)
	}
	var props, h uintptr
	if hr := comCall(data, dpGetProperties, uintptr(unsafe.Pointer(&props))); failed(hr) {
		t.Fatal(hresultError("DataPackage.Properties", hr))
	}
	defer release(props)
	if hr := comCall(props, dppsPutTitle-1, uintptr(unsafe.Pointer(&h))); failed(hr) {
		t.Fatal(hresultError("DataPackagePropertySet.Title", hr))
	}
	if got := hstring(h); got != "Note" {
		t.Errorf("Title = %q, want Note", got)
	}
	winrt.deleteString.Call(h)
}

// TestStorageList walks a storage list as the data package does.
func TestStorageList(t *testing.T) {
	inMTA(t)
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := storageItems([]string{file, file, file})
	if err != nil {
		t.Fatal(err)
	}
	list := newStorageList(storage)
	for _, it := range storage {
		release(it) // the list holds its own
	}
	if q := queryInterface(list, &winrt.iterable); q != list {
		t.Fatalf("QueryInterface(IIterable<IStorageItem>) = %#x, want the list", q)
	} else {
		release(q)
	}
	if q := queryInterface(list, &winrt.iterator); q != 0 {
		t.Error("the list says it is an iterator")
	}
	var it uintptr
	if hr := comCall(list, 6, uintptr(unsafe.Pointer(&it))); failed(hr) { // First
		t.Fatal(hresultError("First", hr))
	}
	release(list) // the iterator holds it
	var has byte
	comCall(it, 7, uintptr(unsafe.Pointer(&has))) // HasCurrent
	var cur uintptr
	if hr := comCall(it, 6, uintptr(unsafe.Pointer(&cur))); has != 1 || failed(hr) || cur != storage[0] {
		t.Fatalf("the first item is %#x (has %d, %#x), want %#x", cur, has, hr, storage[0])
	}
	release(cur)
	comCall(it, 8, uintptr(unsafe.Pointer(&has))) // MoveNext
	var many [4]uintptr
	var n uint32
	comCall(it, 9, 4, uintptr(unsafe.Pointer(&many[0])), uintptr(unsafe.Pointer(&n))) // GetMany
	if has != 1 || n != 2 || many[0] != storage[1] || many[1] != storage[2] {
		t.Errorf("GetMany after MoveNext = %d items %#x, want the last 2", n, many[:n])
	}
	for _, m := range many[:n] {
		release(m)
	}
	if hr := comCall(it, 6, uintptr(unsafe.Pointer(&cur))); hr != eBounds || cur != 0 {
		t.Errorf("Current at the end = %#x, %#x, want E_BOUNDS", cur, hr)
	}
	release(it)
	storageMu.Lock()
	left := len(storageObjects)
	storageMu.Unlock()
	if left != 0 {
		t.Errorf("%d storage objects left after their last release", left)
	}
}
