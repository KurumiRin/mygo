//go:build linux && (amd64 || arm64)

package screencapture

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// GLib's GVariant and GIO's D-Bus, loaded at run time with purego, for
// the portals.

type ptr = uintptr

var (
	glibOnce sync.Once
	glibErr  error

	gFree                                 func(p ptr)
	gErrorFree                            func(e ptr)
	gObjectUnref                          func(o ptr)
	gMainContextNew                       func() ptr
	gMainContextUnref                     func(ctx ptr)
	gMainContextPushThreadDefault         func(ctx ptr)
	gMainContextPopThreadDefault          func(ctx ptr)
	gMainContextIteration                 func(ctx ptr, mayBlock bool) bool
	gMainContextWakeup                    func(ctx ptr)
	gBusGetSync                           func(busType int32, cancellable ptr, err *ptr) ptr
	gDBusConnectionCallSync               func(conn ptr, name, path, iface, method *byte, params ptr, replyType ptr, flags int32, timeout int32, cancellable ptr, err *ptr) ptr
	gDBusConnectionCallWithUnixFDListSync func(conn ptr, name, path, iface, method *byte, params ptr, replyType ptr, flags int32, timeout int32, fdList ptr, outFDList *ptr, cancellable ptr, err *ptr) ptr
	gDBusConnectionSignalSubscribe        func(conn ptr, sender, iface, member, path, arg0 *byte, flags int32, cb ptr, data ptr, free ptr) uint32
	gDBusConnectionSignalUnsubscribe      func(conn ptr, id uint32)
	gDBusConnectionGetUniqueName          func(conn ptr) ptr
	gUnixFDListGet                        func(list ptr, index int32, err *ptr) int32
	gVariantNewString                     func(s *byte) ptr
	gVariantNewObjectPath                 func(s *byte) ptr
	gVariantNewUint32                     func(v uint32) ptr
	gVariantNewBoolean                    func(v bool) ptr
	gVariantNewTuple                      func(children unsafe.Pointer, n uintptr) ptr
	gVariantNewArray                      func(typ ptr, children unsafe.Pointer, n uintptr) ptr
	gVariantNewDictEntry                  func(key, value ptr) ptr
	gVariantNewVariant                    func(v ptr) ptr
	gVariantTypeNew                       func(s *byte) ptr
	gVariantRef                           func(v ptr) ptr
	gVariantUnref                         func(v ptr)
	gVariantGetChildValue                 func(v ptr, i uintptr) ptr
	gVariantNChildren                     func(v ptr) uintptr
	gVariantGetUint32                     func(v ptr) uint32
	gVariantGetInt32                      func(v ptr) int32
	gVariantGetHandle                     func(v ptr) int32
	gVariantGetString                     func(v ptr, length *uintptr) ptr
	gVariantGetVariant                    func(v ptr) ptr
	gVariantGetTypeString                 func(v ptr) ptr
	gVariantLookupValue                   func(dict ptr, key *byte, typ ptr) ptr
)

func loadGLib() error {
	glibOnce.Do(func() {
		glib, err := dlopen("libglib-2.0.so.0")
		if err != nil {
			glibErr = err
			return
		}
		gobject, err := dlopen("libgobject-2.0.so.0")
		if err != nil {
			glibErr = err
			return
		}
		gio, err := dlopen("libgio-2.0.so.0")
		if err != nil {
			glibErr = err
			return
		}
		for _, f := range []struct {
			lib  ptr
			fn   any
			name string
		}{
			{glib, &gFree, "g_free"},
			{glib, &gErrorFree, "g_error_free"},
			{gobject, &gObjectUnref, "g_object_unref"},
			{glib, &gMainContextNew, "g_main_context_new"},
			{glib, &gMainContextUnref, "g_main_context_unref"},
			{glib, &gMainContextPushThreadDefault, "g_main_context_push_thread_default"},
			{glib, &gMainContextPopThreadDefault, "g_main_context_pop_thread_default"},
			{glib, &gMainContextIteration, "g_main_context_iteration"},
			{glib, &gMainContextWakeup, "g_main_context_wakeup"},
			{gio, &gBusGetSync, "g_bus_get_sync"},
			{gio, &gDBusConnectionCallSync, "g_dbus_connection_call_sync"},
			{gio, &gDBusConnectionCallWithUnixFDListSync, "g_dbus_connection_call_with_unix_fd_list_sync"},
			{gio, &gDBusConnectionSignalSubscribe, "g_dbus_connection_signal_subscribe"},
			{gio, &gDBusConnectionSignalUnsubscribe, "g_dbus_connection_signal_unsubscribe"},
			{gio, &gDBusConnectionGetUniqueName, "g_dbus_connection_get_unique_name"},
			{gio, &gUnixFDListGet, "g_unix_fd_list_get"},
			{glib, &gVariantNewString, "g_variant_new_string"},
			{glib, &gVariantNewObjectPath, "g_variant_new_object_path"},
			{glib, &gVariantNewUint32, "g_variant_new_uint32"},
			{glib, &gVariantNewBoolean, "g_variant_new_boolean"},
			{glib, &gVariantNewTuple, "g_variant_new_tuple"},
			{glib, &gVariantNewArray, "g_variant_new_array"},
			{glib, &gVariantNewDictEntry, "g_variant_new_dict_entry"},
			{glib, &gVariantNewVariant, "g_variant_new_variant"},
			{glib, &gVariantTypeNew, "g_variant_type_new"},
			{glib, &gVariantRef, "g_variant_ref"},
			{glib, &gVariantUnref, "g_variant_unref"},
			{glib, &gVariantGetChildValue, "g_variant_get_child_value"},
			{glib, &gVariantNChildren, "g_variant_n_children"},
			{glib, &gVariantGetUint32, "g_variant_get_uint32"},
			{glib, &gVariantGetInt32, "g_variant_get_int32"},
			{glib, &gVariantGetHandle, "g_variant_get_handle"},
			{glib, &gVariantGetString, "g_variant_get_string"},
			{glib, &gVariantGetVariant, "g_variant_get_variant"},
			{glib, &gVariantGetTypeString, "g_variant_get_type_string"},
			{glib, &gVariantLookupValue, "g_variant_lookup_value"},
		} {
			sym, err := purego.Dlsym(f.lib, f.name)
			if err != nil || sym == 0 {
				glibErr = fmt.Errorf("screencapture: missing %s", f.name)
				return
			}
			purego.RegisterFunc(f.fn, sym)
		}
	})
	return glibErr
}

func dlopen(name string) (ptr, error) {
	h, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, fmt.Errorf("screencapture: cannot load %s: %w: %w", name, err, errors.ErrUnsupported)
	}
	return h, nil
}

// cs returns a NUL terminated copy of s for C functions.
func cs(s string) *byte {
	b := make([]byte, len(s)+1)
	copy(b, s)
	return &b[0]
}

// goStr copies a C string.
func goStr(p ptr) string {
	if p == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; ; i++ {
		c := *(*byte)(unsafe.Add(*(*unsafe.Pointer)(unsafe.Pointer(&p)), i))
		if c == 0 {
			return b.String()
		}
		b.WriteByte(c)
	}
}

// gErr converts and frees a GError.
func gErr(e ptr) error {
	if e == 0 {
		return errors.New("screencapture: D-Bus call failed")
	}
	// struct GError { GQuark domain; gint code; gchar *message; }
	msg := goStr(*(*ptr)(unsafe.Add(*(*unsafe.Pointer)(unsafe.Pointer(&e)), 8)))
	gErrorFree(e)
	return errors.New("screencapture: " + msg)
}

func tuple(children ...ptr) ptr {
	if len(children) == 0 {
		return gVariantNewTuple(nil, 0)
	}
	return gVariantNewTuple(unsafe.Pointer(&children[0]), uintptr(len(children)))
}

// vardict builds an a{sv} of entries made by vardictEntry.
func vardict(entries ...ptr) ptr {
	if len(entries) == 0 {
		return gVariantNewArray(gVariantTypeNew(cs("{sv}")), nil, 0)
	}
	return gVariantNewArray(0, unsafe.Pointer(&entries[0]), uintptr(len(entries)))
}

func vardictEntry(key string, v ptr) ptr {
	return gVariantNewDictEntry(gVariantNewString(cs(key)), gVariantNewVariant(v))
}

// lookup returns the value under key in an a{sv} if it has the type typ,
// which the caller unrefs, or 0.
func lookup(dict ptr, key, typ string) ptr {
	if dict == 0 {
		return 0
	}
	v := gVariantLookupValue(dict, cs(key), 0)
	if v == 0 {
		return 0
	}
	if goStr(gVariantGetTypeString(v)) != typ {
		gVariantUnref(v)
		return 0
	}
	return v
}

// lookupString returns the string, or object path, under key in an
// a{sv}, "" when there is none.
func lookupString(dict ptr, key string) string {
	v := lookup(dict, key, "s")
	if v == 0 {
		v = lookup(dict, key, "o")
	}
	if v == 0 {
		return ""
	}
	defer gVariantUnref(v)
	return goStr(gVariantGetString(v, nil))
}
