//go:build windows && (amd64 || arm64)

package windows

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/egoist/mygo/internal/platform"
)

// The share sheet is DataTransferManager's share window, which a desktop
// app reaches through IDataTransferManagerInterop with its window's
// handle. ShowShareUIForWindow makes the manager raise DataRequested, whose
// handler fills the data package with what Share asked for. Files go as
// StorageFiles and StorageFolders, which WinRT makes asynchronously: Share
// makes them on a thread of the MTA first, and hands them, agile objects,
// to the main thread. The data package takes them as an
// IIterable<IStorageItem>, which WinRT has no class for: storageList is one.

// winrt is what sharing calls, which loadWinRT looks up on first use, so
// that apps that never share neither link nor run any of it at init.
var winrt struct {
	once sync.Once
	// combase.dll's functions, and ole32.dll's CoIncrementMTAUsage.
	activationFactory, activateInstance      *syscall.LazyProc
	createString, deleteString, stringBuffer *syscall.LazyProc
	incrementMTA                             *syscall.LazyProc
	// The IIDs of the interfaces.
	dataTransferManagerInterop, dataTransferManager, dataPackage2 GUID
	uriFactory, fileStatics, folderStatics, asyncInfo             GUID
	storageItem, inspectable                                      GUID
	// The IIDs of IIterable<IStorageItem> and IIterator<IStorageItem>,
	// which WinRT derives from their signatures (winrtIID in the test).
	iterable, iterator GUID
}

func loadWinRT() {
	winrt.once.Do(func() {
		combase := systemDLL("combase.dll")
		winrt.activationFactory = combase.NewProc("RoGetActivationFactory")
		winrt.activateInstance = combase.NewProc("RoActivateInstance")
		winrt.createString = combase.NewProc("WindowsCreateString")
		winrt.deleteString = combase.NewProc("WindowsDeleteString")
		winrt.stringBuffer = combase.NewProc("WindowsGetStringRawBuffer")
		winrt.incrementMTA = ole32.NewProc("CoIncrementMTAUsage")
		winrt.dataTransferManagerInterop = guid("3a3dcd6c-3eab-43dc-bcde-45671ce800c8")
		winrt.dataTransferManager = guid("a5caee9b-8708-49d1-8d36-67d25a8da00c")
		winrt.dataPackage2 = guid("041c1fe9-2409-45e1-a538-4c53eeee04a7")
		winrt.uriFactory = guid("44a9796f-723e-4fdf-a218-033e75b0c084")
		winrt.fileStatics = guid("5984c710-daf2-43c8-8bb4-a4d3eacfd03f")
		winrt.folderStatics = guid("08f327ff-85d5-48b9-aee9-28511e339f9f")
		winrt.asyncInfo = guid("00000036-0000-0000-c000-000000000046")
		winrt.storageItem = guid("4207a996-ca2f-42f7-bde8-8b10457a7f30")
		winrt.inspectable = guid("af86e2e0-b12d-4c6a-9c5a-d7aa65101e90")
		winrt.iterable = guid("bb8b8418-65d1-544b-b083-6d172f568c73")
		winrt.iterator = guid("05b487c2-3830-5d3c-98da-25fa11542dbd")
		// The MTA, which storageItems makes the storage items in, stays
		// once the threads that made them leave it.
		var cookie uintptr
		winrt.incrementMTA.Call(uintptr(unsafe.Pointer(&cookie)))
	})
}

// Vtable indices, IInspectable (0 to 5) included, and constants.
const (
	dtmiGetForWindow         = 3 // IDataTransferManagerInterop, an IUnknown
	dtmiShowShareUIForWindow = 4
	dtmAddDataRequested      = 6 // IDataTransferManager
	dtmRemoveDataRequested   = 7
	dreaGetRequest           = 6 // IDataRequestedEventArgs
	drGetData                = 6 // IDataRequest
	drFailWithDisplayText    = 9
	dpGetProperties          = 7 // IDataPackage
	dpSetText                = 16
	dpSetStorageItems        = 22 // SetStorageItemsReadOnly
	dp2SetWebLink            = 7  // IDataPackage2
	dppsPutTitle             = 7  // IDataPackagePropertySet
	uriCreateURI             = 6  // IUriRuntimeClassFactory
	storageFromPathAsync     = 6  // IStorageFileStatics, IStorageFolderStatics
	asyncGetStatus           = 7  // IAsyncInfo
	asyncGetErrorCode        = 8
	asyncGetResults          = 8 // IAsyncOperation<T>

	asyncStarted   = 0 // AsyncStatus
	asyncCompleted = 1

	coinitMultithreaded = 0x0
	eBounds             = 0x8000000b
)

// sharing is what the share window shows: the items, and the files as
// IStorageItems, one reference each.
type sharing struct {
	items   *platform.ShareItems
	storage []uintptr
}

func (s *sharing) release() {
	for _, it := range s.storage {
		release(it)
	}
	s.storage = nil
}

// Share shows the share window for the items. Windows places it itself,
// so the anchor goes unused. It makes the files' storage items on another
// thread first.
func (w *window) Share(items *platform.ShareItems, _ platform.Rect, done func(error)) {
	if w.closed || w.destroying {
		done(errDestroyed)
		return
	}
	if !w.IsVisible() {
		done(errors.New("mygo: Window.Share needs a window that shows"))
		return
	}
	if len(items.Files) == 0 {
		done(w.showShare(&sharing{items: items}))
		return
	}
	go func() {
		storage, err := storageItems(items.Files)
		w.b.post(func() {
			s := &sharing{items: items, storage: storage}
			if err == nil {
				err = w.showShare(s)
			} else {
				s.release()
			}
			done(err)
		})
	}()
}

// showShare makes s what the window shares, and shows the share window,
// with the window's DataTransferManager and its DataRequested handler,
// which the first share adds.
func (w *window) showShare(s *sharing) error {
	if w.closed || w.destroying {
		s.release()
		return errDestroyed
	}
	interop, err := activationFactory("Windows.ApplicationModel.DataTransfer.DataTransferManager", &winrt.dataTransferManagerInterop)
	if err != nil {
		s.release()
		return err
	}
	defer release(interop)
	if w.shareCleanup == nil {
		var dtm uintptr
		if hr := comCall(interop, dtmiGetForWindow, w.hwnd, uintptr(unsafe.Pointer(&winrt.dataTransferManager)), uintptr(unsafe.Pointer(&dtm))); failed(hr) {
			s.release()
			return hresultError("IDataTransferManagerInterop::GetForWindow", hr)
		}
		h := newAgileHandler(func(_, args uintptr) { w.dataRequested(args) })
		var token int64
		hr := comCall(dtm, dtmAddDataRequested, h, uintptr(unsafe.Pointer(&token)))
		release(h)
		if failed(hr) {
			release(dtm)
			s.release()
			return hresultError("DataTransferManager.DataRequested", hr)
		}
		w.shareCleanup = func() {
			comCall(dtm, dtmRemoveDataRequested, uintptr(token))
			release(dtm)
			if w.sharing != nil {
				w.sharing.release()
				w.sharing = nil
			}
		}
	}
	if w.sharing != nil {
		w.sharing.release()
	}
	w.sharing = s
	if hr := comCall(interop, dtmiShowShareUIForWindow, w.hwnd); failed(hr) {
		return hresultError("IDataTransferManagerInterop::ShowShareUIForWindow", hr)
	}
	return nil
}

// dataRequested fills the data package of a DataRequested event with what
// the window shares, or makes the share window tell why it cannot.
func (w *window) dataRequested(args uintptr) {
	s := w.sharing
	var req uintptr
	if s == nil || failed(comCall(args, dreaGetRequest, uintptr(unsafe.Pointer(&req)))) {
		return
	}
	defer release(req)
	var data uintptr
	if failed(comCall(req, drGetData, uintptr(unsafe.Pointer(&data)))) {
		return
	}
	defer release(data)
	if err := fillDataPackage(data, s); err != nil {
		withHString(err.Error(), func(h uintptr) uintptr { return comCall(req, drFailWithDisplayText, h) })
	}
}

func fillDataPackage(data uintptr, s *sharing) error {
	var props uintptr
	if hr := comCall(data, dpGetProperties, uintptr(unsafe.Pointer(&props))); failed(hr) {
		return hresultError("DataPackage.Properties", hr)
	}
	hr := withHString(s.items.Title, func(h uintptr) uintptr { return comCall(props, dppsPutTitle, h) })
	release(props)
	if failed(hr) {
		return hresultError("DataPackagePropertySet.Title", hr)
	}
	if s.items.Text != "" {
		if hr := withHString(s.items.Text, func(h uintptr) uintptr { return comCall(data, dpSetText, h) }); failed(hr) {
			return hresultError("DataPackage.SetText", hr)
		}
	}
	if s.items.URL != "" {
		if err := setWebLink(data, s.items.URL); err != nil {
			return err
		}
	}
	if len(s.storage) > 0 {
		list := newStorageList(s.storage)
		hr := comCall(data, dpSetStorageItems, list)
		release(list)
		if failed(hr) {
			return hresultError("DataPackage.SetStorageItems", hr)
		}
	}
	return nil
}

func setWebLink(data uintptr, link string) error {
	factory, err := activationFactory("Windows.Foundation.Uri", &winrt.uriFactory)
	if err != nil {
		return err
	}
	defer release(factory)
	var uri uintptr
	if hr := withHString(link, func(h uintptr) uintptr { return comCall(factory, uriCreateURI, h, uintptr(unsafe.Pointer(&uri))) }); failed(hr) {
		return hresultError("Uri.CreateUri", hr)
	}
	defer release(uri)
	dp2 := queryInterface(data, &winrt.dataPackage2)
	if dp2 == 0 {
		return errors.New("mygo: the data package has no IDataPackage2")
	}
	defer release(dp2)
	if hr := comCall(dp2, dp2SetWebLink, uri); failed(hr) {
		return hresultError("DataPackage.SetWebLink", hr)
	}
	return nil
}

// storageItems makes the StorageFile or StorageFolder of each path, as
// IStorageItems, on a thread of the MTA.
func storageItems(paths []string) ([]uintptr, error) {
	loadWinRT()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if r, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded); !failed(r) {
		defer procCoUninitialize.Call()
	}
	var items []uintptr
	fail := func(err error) ([]uintptr, error) {
		for _, it := range items {
			release(it)
		}
		return nil, err
	}
	for _, p := range paths {
		class, iid, what := "Windows.Storage.StorageFile", &winrt.fileStatics, "StorageFile.GetFileFromPathAsync"
		if fi, err := os.Stat(p); err != nil {
			return fail(err)
		} else if fi.IsDir() {
			class, iid, what = "Windows.Storage.StorageFolder", &winrt.folderStatics, "StorageFolder.GetFolderFromPathAsync"
		}
		statics, err := activationFactory(class, iid)
		if err != nil {
			return fail(err)
		}
		var op uintptr
		hr := withHString(p, func(h uintptr) uintptr {
			return comCall(statics, storageFromPathAsync, h, uintptr(unsafe.Pointer(&op)))
		})
		release(statics)
		if failed(hr) {
			return fail(hresultError(what, hr))
		}
		item, err := storageResult(op, what)
		release(op)
		if err != nil {
			return fail(err)
		}
		items = append(items, item)
	}
	return items, nil
}

// storageResult waits for an IAsyncOperation of a StorageFile or a
// StorageFolder and returns its IStorageItem. The thread has nothing else
// to do, so it polls the operation rather than making a completion handler.
func storageResult(op uintptr, what string) (uintptr, error) {
	info := queryInterface(op, &winrt.asyncInfo)
	if info == 0 {
		return 0, errors.New("mygo: " + what + " is not asynchronous")
	}
	defer release(info)
	var status int32
	for start := time.Now(); ; time.Sleep(time.Millisecond) {
		if hr := comCall(info, asyncGetStatus, uintptr(unsafe.Pointer(&status))); failed(hr) {
			return 0, hresultError(what, hr)
		}
		if status != asyncStarted {
			break
		}
		if time.Since(start) > 30*time.Second {
			return 0, errors.New("mygo: " + what + " timed out")
		}
	}
	if status != asyncCompleted {
		var code int32
		comCall(info, asyncGetErrorCode, uintptr(unsafe.Pointer(&code)))
		return 0, hresultError(what, uintptr(uint32(code)))
	}
	var res uintptr
	if hr := comCall(op, asyncGetResults, uintptr(unsafe.Pointer(&res))); failed(hr) {
		return 0, hresultError(what, hr)
	}
	defer release(res)
	item := queryInterface(res, &winrt.storageItem)
	if item == 0 {
		return 0, errors.New("mygo: " + what + " gave no IStorageItem")
	}
	return item, nil
}

// activationFactory returns the iid interface of the activation factory of
// a WinRT class.
func activationFactory(class string, iid *GUID) (uintptr, error) {
	loadWinRT()
	var f uintptr
	hr := withHString(class, func(h uintptr) uintptr {
		r, _, _ := winrt.activationFactory.Call(h, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&f)))
		return r
	})
	if failed(hr) {
		return 0, hresultError("RoGetActivationFactory("+class+")", hr)
	}
	return f, nil
}

// withHString calls call with an HSTRING of s, which WinRT methods copy
// when they keep it, and returns what it returns.
func withHString(s string, call func(h uintptr) uintptr) uintptr {
	loadWinRT()
	u := utf16z(s)
	var h uintptr
	if hr, _, _ := winrt.createString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h))); failed(hr) {
		return hr
	}
	defer winrt.deleteString.Call(h)
	return call(h)
}

// A storageList is an IIterable<IStorageItem> of storage items, and a
// storageIterator its IIterator. Both are agile and may be called from any
// thread: the data package can read them from another.
type storageObject struct {
	vtbl *uintptr
	refs int32
	iid  GUID
	// items are the list's, one reference each; an iterator holds a
	// reference to its list and reads its items from next.
	items []uintptr
	list  uintptr
	next  int
}

var (
	storageMu       sync.Mutex
	storageObjects  map[uintptr]*storageObject
	storageListVtbl [7]uintptr
	storageIterVtbl [10]uintptr
	storageOnce     sync.Once
)

func initStorageVtbl() {
	storageOnce.Do(func() {
		inspectable := [6]uintptr{
			syscall.NewCallback(func(this, riid, out uintptr) uintptr {
				storageMu.Lock()
				o := storageObjects[this]
				storageMu.Unlock()
				iid := *(*GUID)(native(riid))
				if o == nil || iid != iidIUnknown && iid != winrt.inspectable && iid != iidIAgileObject && iid != o.iid {
					*(*uintptr)(native(out)) = 0
					return eNoInterface
				}
				storageRef(this, 1)
				*(*uintptr)(native(out)) = this
				return sOK
			}),
			syscall.NewCallback(func(this uintptr) uintptr { return uintptr(storageRef(this, 1)) }),
			syscall.NewCallback(func(this uintptr) uintptr { return uintptr(storageRef(this, -1)) }),
			syscall.NewCallback(func(this, count, iids uintptr) uintptr { // GetIids
				*(*uint32)(native(count)) = 0
				*(*uintptr)(native(iids)) = 0
				return sOK
			}),
			syscall.NewCallback(func(this, name uintptr) uintptr { *(*uintptr)(native(name)) = 0; return sOK }), // GetRuntimeClassName
			syscall.NewCallback(func(this, level uintptr) uintptr { *(*int32)(native(level)) = 0; return sOK }), // GetTrustLevel: BaseTrust
		}
		copy(storageListVtbl[:], inspectable[:])
		storageListVtbl[6] = syscall.NewCallback(func(this, out uintptr) uintptr { // First
			addRef(this)
			*(*uintptr)(native(out)) = newStorageObject(&storageIterVtbl[0], winrt.iterator, nil, this)
			return sOK
		})
		copy(storageIterVtbl[:], inspectable[:])
		storageIterVtbl[6] = syscall.NewCallback(func(this, out uintptr) uintptr { // get_Current
			item := storageNext(this, false)
			*(*uintptr)(native(out)) = item
			if item == 0 {
				return eBounds
			}
			return sOK
		})
		storageIterVtbl[7] = syscall.NewCallback(func(this, out uintptr) uintptr { // get_HasCurrent
			*(*byte)(native(out)) = boolByte(storageNext(this, false) != 0)
			return sOK
		})
		storageIterVtbl[8] = syscall.NewCallback(func(this, out uintptr) uintptr { // MoveNext
			release(storageNext(this, true))
			*(*byte)(native(out)) = boolByte(storageNext(this, false) != 0)
			return sOK
		})
		storageIterVtbl[9] = syscall.NewCallback(func(this, capacity, items, actual uintptr) uintptr { // GetMany
			n := 0
			for ; n < int(uint32(capacity)); n++ {
				item := storageNext(this, true)
				if item == 0 {
					break
				}
				*(*uintptr)(unsafe.Add(native(items), n*int(unsafe.Sizeof(uintptr(0))))) = item
			}
			*(*uint32)(native(actual)) = uint32(n)
			return sOK
		})
	})
}

// newStorageList returns a storage list of items, holding one reference,
// and references of its own to the items.
func newStorageList(items []uintptr) uintptr {
	loadWinRT()
	initStorageVtbl()
	for _, it := range items {
		addRef(it)
	}
	return newStorageObject(&storageListVtbl[0], winrt.iterable, items, 0)
}

func newStorageObject(vtbl *uintptr, iid GUID, items []uintptr, list uintptr) uintptr {
	o := &storageObject{vtbl: vtbl, refs: 1, iid: iid, items: items, list: list}
	p := uintptr(unsafe.Pointer(o))
	storageMu.Lock()
	if storageObjects == nil {
		storageObjects = map[uintptr]*storageObject{}
	}
	storageObjects[p] = o
	storageMu.Unlock()
	return p
}

func storageRef(this uintptr, delta int32) int32 {
	storageMu.Lock()
	o := storageObjects[this]
	if o == nil {
		storageMu.Unlock()
		return 0
	}
	o.refs += delta
	refs := o.refs
	if refs > 0 {
		storageMu.Unlock()
		return refs
	}
	delete(storageObjects, this)
	storageMu.Unlock()
	for _, it := range o.items {
		release(it)
	}
	release(o.list)
	return 0
}

// storageNext returns the current item of an iterator, with a reference,
// or 0 at the end; advance moves the iterator past it.
func storageNext(this uintptr, advance bool) uintptr {
	storageMu.Lock()
	defer storageMu.Unlock()
	it := storageObjects[this]
	if it == nil {
		return 0
	}
	list := storageObjects[it.list]
	if list == nil || it.next >= len(list.items) {
		return 0
	}
	item := list.items[it.next]
	if advance {
		it.next++
	}
	addRef(item)
	return item
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
