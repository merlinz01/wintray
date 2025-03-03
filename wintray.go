//go:build windows

// Package wintray is a Windows-specific Go library to place an icon and menu in the notification area.
package wintray

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// Callback function to be called when the systray is ready
	systrayReady func()
	// Callback function to be called when the systray is exited
	systrayExit func()
	// Ensures systrayExit is called only once
	systrayExitOnce sync.Once
	// Map of menu item ID's to their respective MenuItem objects
	menuItems = make(map[uint32]*MenuItem)
	// Lock to protect menuItems
	menuItemsLock sync.RWMutex
	// ID to assign to the next menu item
	currentID atomic.Uint32
	// Ensures Quit is called only once
	quitOnce sync.Once
	// Callbacks to be called when the tray is opened
	trayOpenedCallbacks []func()
	// Whether or not the icon should respond to left/right clicks
	openOnLeftClick  = true
	openOnRightClick = true
)

var (
	g32                     = windows.NewLazySystemDLL("Gdi32.dll")
	pCreateCompatibleBitmap = g32.NewProc("CreateCompatibleBitmap")
	pCreateCompatibleDC     = g32.NewProc("CreateCompatibleDC")
	pCreateDIBSection       = g32.NewProc("CreateDIBSection")
	pDeleteDC               = g32.NewProc("DeleteDC")
	pSelectObject           = g32.NewProc("SelectObject")

	k32              = windows.NewLazySystemDLL("Kernel32.dll")
	pGetModuleHandle = k32.NewProc("GetModuleHandleW")

	s32              = windows.NewLazySystemDLL("Shell32.dll")
	pShellNotifyIcon = s32.NewProc("Shell_NotifyIconW")

	u32                    = windows.NewLazySystemDLL("User32.dll")
	pCreateMenu            = u32.NewProc("CreateMenu")
	pCreatePopupMenu       = u32.NewProc("CreatePopupMenu")
	pCreateWindowEx        = u32.NewProc("CreateWindowExW")
	pDefWindowProc         = u32.NewProc("DefWindowProcW")
	pDeleteMenu            = u32.NewProc("DeleteMenu")
	pDestroyMenu           = u32.NewProc("DestroyMenu")
	pRemoveMenu            = u32.NewProc("RemoveMenu")
	pDestroyWindow         = u32.NewProc("DestroyWindow")
	pDispatchMessage       = u32.NewProc("DispatchMessageW")
	pDrawIconEx            = u32.NewProc("DrawIconEx")
	pGetCursorPos          = u32.NewProc("GetCursorPos")
	pGetDC                 = u32.NewProc("GetDC")
	pGetMessage            = u32.NewProc("GetMessageW")
	pGetSystemMetrics      = u32.NewProc("GetSystemMetrics")
	pInsertMenuItem        = u32.NewProc("InsertMenuItemW")
	pLoadCursor            = u32.NewProc("LoadCursorW")
	pLoadIcon              = u32.NewProc("LoadIconW")
	pLoadImage             = u32.NewProc("LoadImageW")
	pPostMessage           = u32.NewProc("PostMessageW")
	pPostQuitMessage       = u32.NewProc("PostQuitMessage")
	pRegisterClass         = u32.NewProc("RegisterClassExW")
	pRegisterWindowMessage = u32.NewProc("RegisterWindowMessageW")
	pReleaseDC             = u32.NewProc("ReleaseDC")
	pSetForegroundWindow   = u32.NewProc("SetForegroundWindow")
	pSetMenuInfo           = u32.NewProc("SetMenuInfo")
	pSetMenuItemInfo       = u32.NewProc("SetMenuItemInfoW")
	pShowWindow            = u32.NewProc("ShowWindow")
	pTrackPopupMenu        = u32.NewProc("TrackPopupMenu")
	pTranslateMessage      = u32.NewProc("TranslateMessage")
	pUnregisterClass       = u32.NewProc("UnregisterClassW")
	pUpdateWindow          = u32.NewProc("UpdateWindow")

	// ErrTrayNotReadyYet is returned by functions when they are called before the tray has been initialized.
	ErrTrayNotReadyYet = errors.New("tray not ready yet")
)

// Lock the OS thread to ensure that the message loop runs on the main thread.
func init() {
	runtime.LockOSThread()
}

// Add a callback to be called when the tray is opened.
func OnTrayOpened(f func()) {
	trayOpenedCallbacks = append(trayOpenedCallbacks, f)
}

// Set whether or not the icon should respond to left clicks.
// The default is true.
func SetOpenOnLeftClick(open bool) {
	openOnLeftClick = open
}

// Set whether or not the icon should respond to right clicks.
// The default is true.
func SetOpenOnRightClick(open bool) {
	openOnRightClick = open
}

// MenuItem is used to keep track each menu item of systray.
// Don't create it directly, use systray.AddMenuItem()
type MenuItem struct {
	// Callback function to be called when the menu item is clicked
	onClick func()

	// Unique identifier for the menu item; not to be modified
	id uint32
	// The text shown on the menu item
	title string
	// Whether or not the menu item is disabled
	disabled bool
	// Whether or not the menu item is checked
	checked bool
	// Parent menu item, for submenus
	parent *MenuItem
}

// Return a string representation of the MenuItem for debugging
func (item *MenuItem) String() string {
	if item.parent == nil {
		return fmt.Sprintf("MenuItem[%d, %q]", item.id, item.title)
	}
	return fmt.Sprintf("MenuItem[%d, parent %d, %q]", item.id, item.parent.id, item.title)
}

// Return a populated MenuItem object.
func newMenuItem(title string, parent *MenuItem) *MenuItem {
	return &MenuItem{
		onClick:  nil,
		id:       currentID.Add(1),
		title:    title,
		disabled: false,
		checked:  false,
		parent:   parent,
	}
}

// Initialize the GUI and start the event loop, then invoke the onReady
// callback. Blocks until systray.Quit() is called.
func Run(onReady, onExit func()) error {
	err := Register(onReady, onExit)
	if err != nil {
		return err
	}
	nativeLoop()
	return nil
}

// RunWithExternalLoop allows the systemtray module to operate with other tookits.
// The returned start and end functions should be called by the toolkit when the application has started and will end.
func RunWithExternalLoop(onReady, onExit func()) (start, end func(), err error) {
	err = Register(onReady, onExit)
	if err != nil {
		return nil, nil, err
	}
	return func() { go nativeLoop() }, Quit, nil
}

// Initializes the GUI and register the callbacks. Relies on the
// caller to run the event loop somewhere else. Useful if the program
// needs to show other UI elements.
func Register(onReady func(), onExit func()) error {
	if onReady == nil {
		systrayReady = func() {}
	} else {
		// Run onReady on separate goroutine to avoid blocking event loop
		readyCh := make(chan interface{})
		go func() {
			<-readyCh
			onReady()
		}()
		systrayReady = func() {
			close(readyCh)
		}
	}
	// unlike onReady, onExit runs in the event loop to make sure it has time to
	// finish before the process terminates
	if onExit == nil {
		onExit = func() {}
	}
	systrayExit = onExit
	if err := wt.initInstance(); err != nil {
		return fmt.Errorf("unable to initialize systray: %w", err)
	}

	if err := wt.createMenu(); err != nil {
		return fmt.Errorf("unable to create menu: %w", err)
	}

	wt.initialized.Store(true)
	systrayReady()
	return nil
}

// Remove all menu items.
func ResetMenu() {
	menuItemsLock.Lock()
	id := currentID.Load()
	menuItemsLock.Unlock()
	for i, item := range menuItems {
		if i < id {
			item.Remove()
		}
	}
	_, _, err := pDestroyMenu.Call(uintptr(wt.menus[0]))
	if err != nil {
		log.Printf("systray error: failed to destroy menu: %s\n", err)
	}
	wt.visibleItems = make(map[uint32][]uint32)
	wt.menus = make(map[uint32]windows.Handle)
	wt.menuOf = make(map[uint32]windows.Handle)
	wt.menuItemIcons = make(map[uint32]windows.Handle)
	err = wt.createMenu()
	if err != nil {
		log.Printf("systray error: failed to create menu: %s\n", err)
	}
}

// Quit the systray message loop.
func Quit() {
	quitOnce.Do(quit)
}

// Add a menu item with the designated title.
// Can be safely invoked from different goroutines.
func AddMenuItem(title string) *MenuItem {
	item := newMenuItem(title, nil)
	item.update()
	return item
}

// Set the function to be called when the menu item is clicked.
// The function is called from a new goroutine.
func (item *MenuItem) SetCallback(onClick func()) {
	item.onClick = onClick
}

// Add a separator bar to the menu.
func AddSeparator() {
	addSeparator(currentID.Add(1), 0)
}

// Add a separator bar to the submenu.
func (item *MenuItem) AddSeparator() {
	addSeparator(currentID.Add(1), item.id)
}

// Add a nested sub-menu item with the designated title.
// Can be safely invoked from different goroutines.
func (item *MenuItem) AddSubMenuItem(title string) *MenuItem {
	child := newMenuItem(title, item)
	child.update()
	return child
}

// Set the text to display on a menu item.
func (item *MenuItem) SetTitle(title string) {
	item.title = title
	item.update()
}

// Return whether the menu item is disabled.
func (item *MenuItem) Disabled() bool {
	return item.disabled
}

// Enable a menu item regardless if it's previously enabled or not.
func (item *MenuItem) Enable() {
	item.disabled = false
	item.update()
}

// Disable a menu item regardless if it's previously disabled or not.
func (item *MenuItem) Disable() {
	item.disabled = true
	item.update()
}

// Hide a menu item.
func (item *MenuItem) Hide() {
	err := wt.hideMenuItem(uint32(item.id), item.parentId())
	if err != nil {
		log.Printf("systray error: failed to hide menu item: %s\n", err)
	}
}

// Remove a menu item and, if it has a submenu, all its children.
func (item *MenuItem) Remove() {
	// Delete all children first
	menuItemsLock.RLock()
	childList := make([]*MenuItem, 0, len(menuItems))
	for _, child := range menuItems {
		if child.parent == item {
			childList = append(childList, child)
		}
	}
	menuItemsLock.RUnlock()
	for _, child := range childList {
		child.Remove()
	}
	err := wt.removeMenuItem(uint32(item.id), item.parentId())
	if err != nil {
		log.Printf("systray error: unable to removeMenuItem: %s\n", err)
	}
	menuItemsLock.Lock()
	delete(menuItems, item.id)
	menuItemsLock.Unlock()
}

// Show a previously hidden menu item.
func (item *MenuItem) Show() {
	addOrUpdateMenuItem(item)
}

// Return if the menu item has a check mark.
func (item *MenuItem) Checked() bool {
	return item.checked
}

// Check a menu item regardless if it's previously checked or not.
func (item *MenuItem) Check() {
	item.checked = true
	item.update()
}

// Uncheck a menu item regardless if it's previously unchecked or not.
func (item *MenuItem) Uncheck() {
	item.checked = false
	item.update()
}

// Update a menu item with new properties.
func (item *MenuItem) update() {
	menuItemsLock.Lock()
	menuItems[item.id] = item
	menuItemsLock.Unlock()
	addOrUpdateMenuItem(item)
}

// Contains information about loaded resources
type winTray struct {
	instance,
	icon,
	cursor,
	window windows.Handle

	loadedImages   map[string]windows.Handle
	muLoadedImages sync.RWMutex
	// menus keeps track of the submenus keyed by the menu item ID, plus 0
	// which corresponds to the main popup menu.
	menus   map[uint32]windows.Handle
	muMenus sync.RWMutex
	// menuOf keeps track of the menu each menu item belongs to.
	menuOf   map[uint32]windows.Handle
	muMenuOf sync.RWMutex
	// menuItemIcons maintains the bitmap of each menu item (if applies). It's
	// needed to show the icon correctly when showing a previously hidden menu
	// item again.
	menuItemIcons   map[uint32]windows.Handle
	muMenuItemIcons sync.RWMutex
	visibleItems    map[uint32][]uint32
	muVisibleItems  sync.RWMutex

	nid   *notifyIconData
	muNID sync.RWMutex
	wcex  *wndClassEx

	wmSystrayMessage,
	wmTaskbarCreated uint32

	initialized atomic.Bool
}

var wt = winTray{}

// Check if the tray as already been initialized.
// Not goroutine safe with in regard to the initialization function,
// but prevents a panic when functions are called too early.
func (t *winTray) isReady() bool {
	return t.initialized.Load()
}

// Loads an image from file and shows it in the tray.
// Shell_NotifyIcon: https://msdn.microsoft.com/en-us/library/windows/desktop/bb762159(v=vs.85).aspx
func (t *winTray) setIcon(src string) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const NIF_ICON = 0x00000002

	h, err := t.loadIconFrom(src)
	if err != nil {
		return err
	}

	t.muNID.Lock()
	defer t.muNID.Unlock()
	t.nid.Icon = h
	t.nid.Flags |= NIF_ICON
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))

	return t.nid.modify()
}

// WindowProc callback function that processes messages sent to a window.
// https://msdn.microsoft.com/en-us/library/windows/desktop/ms633573(v=vs.85).aspx
func (t *winTray) wndProc(hWnd windows.Handle, message uint32, wParam, lParam uintptr) (lResult uintptr) {
	const (
		WM_RBUTTONUP  = 0x0205
		WM_LBUTTONUP  = 0x0202
		WM_COMMAND    = 0x0111
		WM_ENDSESSION = 0x0016
		WM_CLOSE      = 0x0010
		WM_DESTROY    = 0x0002
	)
	switch message {
	case WM_COMMAND:
		menuItemId := int32(wParam)
		// https://docs.microsoft.com/en-us/windows/win32/menurc/wm-command#menus
		if menuItemId != -1 {
			id := uint32(wParam)
			menuItemsLock.RLock()
			item, ok := menuItems[id]
			menuItemsLock.RUnlock()
			if !ok {
				log.Printf("systray error: no menu item with ID %d\n", id)
				return
			}
			if item.onClick != nil {
				go item.onClick()
			}
		}
	case WM_CLOSE:
		pDestroyWindow.Call(uintptr(t.window))
		t.wcex.unregister()
	case WM_DESTROY:
		// same as WM_ENDSESSION, but throws 0 exit code after all
		defer pPostQuitMessage.Call(uintptr(int32(0)))
		fallthrough
	case WM_ENDSESSION:
		t.muNID.Lock()
		if t.nid != nil {
			t.nid.delete()
		}
		t.muNID.Unlock()
		systrayExitOnce.Do(systrayExit)
	case t.wmSystrayMessage:
		if (lParam == WM_RBUTTONUP && openOnRightClick) ||
			(lParam == WM_LBUTTONUP && openOnLeftClick) {
			for _, f := range trayOpenedCallbacks {
				f()
			}
			t.showMenu()
		}
	case t.wmTaskbarCreated: // on explorer.exe restarts
		t.muNID.Lock()
		t.nid.add()
		t.muNID.Unlock()
	default:
		// Calls the default window procedure to provide default processing for any window messages that an application does not process.
		// https://msdn.microsoft.com/en-us/library/windows/desktop/ms633572(v=vs.85).aspx
		lResult, _, _ = pDefWindowProc.Call(
			uintptr(hWnd),
			uintptr(message),
			uintptr(wParam),
			uintptr(lParam),
		)
	}
	return
}

// Register the window class and create the window for the event loop.
func (t *winTray) initInstance() error {
	const IDI_APPLICATION = 32512
	const IDC_ARROW = 32512 // Standard arrow
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms633548(v=vs.85).aspx
	const SW_HIDE = 0
	const CW_USEDEFAULT = 0x80000000
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms632600(v=vs.85).aspx
	const (
		WS_CAPTION     = 0x00C00000
		WS_MAXIMIZEBOX = 0x00010000
		WS_MINIMIZEBOX = 0x00020000
		WS_OVERLAPPED  = 0x00000000
		WS_SYSMENU     = 0x00080000
		WS_THICKFRAME  = 0x00040000

		WS_OVERLAPPEDWINDOW = WS_OVERLAPPED | WS_CAPTION | WS_SYSMENU | WS_THICKFRAME | WS_MINIMIZEBOX | WS_MAXIMIZEBOX
	)
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ff729176
	const (
		CS_HREDRAW = 0x0002
		CS_VREDRAW = 0x0001
	)
	const NIF_MESSAGE = 0x00000001

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms644931(v=vs.85).aspx
	const WM_USER = 0x0400

	const (
		className  = "SystrayClass"
		windowName = ""
	)

	t.wmSystrayMessage = WM_USER + 1
	t.visibleItems = make(map[uint32][]uint32)
	t.menus = make(map[uint32]windows.Handle)
	t.menuOf = make(map[uint32]windows.Handle)
	t.menuItemIcons = make(map[uint32]windows.Handle)

	taskbarEventNamePtr, _ := windows.UTF16PtrFromString("TaskbarCreated")
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms644947
	res, _, err := pRegisterWindowMessage.Call(
		uintptr(unsafe.Pointer(taskbarEventNamePtr)),
	)
	t.wmTaskbarCreated = uint32(res)

	t.loadedImages = make(map[string]windows.Handle)

	instanceHandle, _, err := pGetModuleHandle.Call(0)
	if instanceHandle == 0 {
		return fmt.Errorf("failed to get executable module handle: %w", err)
	}
	t.instance = windows.Handle(instanceHandle)

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms648072(v=vs.85).aspx
	iconHandle, _, err := pLoadIcon.Call(0, uintptr(IDI_APPLICATION))
	if iconHandle == 0 {
		return fmt.Errorf("failed to load icon: %w", err)
	}
	t.icon = windows.Handle(iconHandle)

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms648391(v=vs.85).aspx
	cursorHandle, _, err := pLoadCursor.Call(0, uintptr(IDC_ARROW))
	if cursorHandle == 0 {
		return fmt.Errorf("failed to load cursor: %w", err)
	}
	t.cursor = windows.Handle(cursorHandle)

	classNamePtr, err := windows.UTF16PtrFromString(className)
	if err != nil {
		return err
	}

	windowNamePtr, err := windows.UTF16PtrFromString(windowName)
	if err != nil {
		return err
	}

	t.wcex = &wndClassEx{
		Style:      CS_HREDRAW | CS_VREDRAW,
		WndProc:    windows.NewCallback(t.wndProc),
		Instance:   t.instance,
		Icon:       t.icon,
		Cursor:     t.cursor,
		Background: windows.Handle(6), // (COLOR_WINDOW + 1)
		ClassName:  classNamePtr,
		IconSm:     t.icon,
	}
	if err := t.wcex.register(); err != nil {
		return fmt.Errorf("failed to register window class: %w", err)
	}

	windowHandle, _, err := pCreateWindowEx.Call(
		uintptr(0),
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(windowNamePtr)),
		uintptr(WS_OVERLAPPEDWINDOW),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(0),
		uintptr(0),
		uintptr(t.instance),
		uintptr(0),
	)
	if windowHandle == 0 {
		return fmt.Errorf("failed to create window: %w", err)
	}
	t.window = windows.Handle(windowHandle)

	pShowWindow.Call(
		uintptr(t.window),
		uintptr(SW_HIDE),
	)

	pUpdateWindow.Call(
		uintptr(t.window),
	)

	t.muNID.Lock()
	defer t.muNID.Unlock()
	t.nid = &notifyIconData{
		Wnd:             windows.Handle(t.window),
		ID:              100,
		Flags:           NIF_MESSAGE,
		CallbackMessage: t.wmSystrayMessage,
	}
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))

	err = t.nid.add()
	if err != nil {
		return fmt.Errorf("failed to create taskbar icon: %w", err)
	}
	return nil
}

// Create the main popup menu.
func (t *winTray) createMenu() error {
	const MIM_APPLYTOSUBMENUS = 0x80000000 // Settings apply to the menu and all of its submenus

	menuHandle, _, err := pCreatePopupMenu.Call()
	if menuHandle == 0 {
		return err
	}
	t.menus[0] = windows.Handle(menuHandle)

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647575(v=vs.85).aspx
	mi := struct {
		Size, Mask, Style, Max uint32
		Background             windows.Handle
		ContextHelpID          uint32
		MenuData               uintptr
	}{
		Mask: MIM_APPLYTOSUBMENUS,
	}
	mi.Size = uint32(unsafe.Sizeof(mi))

	res, _, err := pSetMenuInfo.Call(
		uintptr(t.menus[0]),
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return err
	}
	return nil
}

// Create a submenu for a menu item.
func (t *winTray) convertToSubMenu(menuItemId uint32) (windows.Handle, error) {
	const MIIM_SUBMENU = 0x00000004

	res, _, err := pCreateMenu.Call()
	if res == 0 {
		return 0, err
	}
	menu := windows.Handle(res)

	mi := menuItemInfo{Mask: MIIM_SUBMENU, SubMenu: menu}
	mi.Size = uint32(unsafe.Sizeof(mi))
	t.muMenuOf.RLock()
	hMenu := t.menuOf[menuItemId]
	t.muMenuOf.RUnlock()
	res, _, err = pSetMenuItemInfo.Call(
		uintptr(hMenu),
		uintptr(menuItemId),
		0,
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return 0, err
	}
	t.muMenus.Lock()
	t.menus[menuItemId] = menu
	t.muMenus.Unlock()
	return menu, nil
}

// Add or update a menu item.
func (t *winTray) addOrUpdateMenuItem(menuItemId uint32, parentId uint32, title string, disabled, checked bool) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647578(v=vs.85).aspx
	const (
		MIIM_FTYPE   = 0x00000100
		MIIM_BITMAP  = 0x00000080
		MIIM_STRING  = 0x00000040
		MIIM_SUBMENU = 0x00000004
		MIIM_ID      = 0x00000002
		MIIM_STATE   = 0x00000001
	)
	const MFT_STRING = 0x00000000
	const (
		MFS_CHECKED  = 0x00000008
		MFS_DISABLED = 0x00000003
	)
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}

	mi := menuItemInfo{
		Mask:     MIIM_FTYPE | MIIM_STRING | MIIM_ID | MIIM_STATE,
		Type:     MFT_STRING,
		ID:       uint32(menuItemId),
		TypeData: titlePtr,
		Cch:      uint32(len(title)),
	}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if disabled {
		mi.State |= MFS_DISABLED
	}
	if checked {
		mi.State |= MFS_CHECKED
	}
	t.muMenuItemIcons.RLock()
	hIcon := t.menuItemIcons[menuItemId]
	t.muMenuItemIcons.RUnlock()
	if hIcon > 0 {
		mi.Mask |= MIIM_BITMAP
		mi.BMPItem = hIcon
	}

	var res uintptr
	t.muMenus.RLock()
	menu, exists := t.menus[parentId]
	t.muMenus.RUnlock()
	if !exists {
		menu, err = t.convertToSubMenu(parentId)
		if err != nil {
			return err
		}
		t.muMenus.Lock()
		t.menus[parentId] = menu
		t.muMenus.Unlock()
	} else if t.getVisibleItemIndex(parentId, menuItemId) != -1 {
		// We set the menu item info based on the menuID
		res, _, err = pSetMenuItemInfo.Call(
			uintptr(menu),
			uintptr(menuItemId),
			0,
			uintptr(unsafe.Pointer(&mi)),
		)
	}

	if res == 0 {
		// Menu item does not already exist, create it
		t.muMenus.RLock()
		submenu, exists := t.menus[menuItemId]
		t.muMenus.RUnlock()
		if exists {
			mi.Mask |= MIIM_SUBMENU
			mi.SubMenu = submenu
		}
		t.addToVisibleItems(parentId, menuItemId)
		position := t.getVisibleItemIndex(parentId, menuItemId)
		res, _, err = pInsertMenuItem.Call(
			uintptr(menu),
			uintptr(position),
			1,
			uintptr(unsafe.Pointer(&mi)),
		)
		if res == 0 {
			t.delFromVisibleItems(parentId, menuItemId)
			return err
		}
		t.muMenuOf.Lock()
		t.menuOf[menuItemId] = menu
		t.muMenuOf.Unlock()
	}

	return nil
}

// Add a separator to the menu.
func (t *winTray) addSeparatorMenuItem(menuItemId, parentId uint32) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647578(v=vs.85).aspx
	const (
		MIIM_FTYPE = 0x00000100
		MIIM_ID    = 0x00000002
		MIIM_STATE = 0x00000001
	)
	const MFT_SEPARATOR = 0x00000800

	mi := menuItemInfo{
		Mask: MIIM_FTYPE | MIIM_ID | MIIM_STATE,
		Type: MFT_SEPARATOR,
		ID:   uint32(menuItemId),
	}

	mi.Size = uint32(unsafe.Sizeof(mi))

	t.addToVisibleItems(parentId, menuItemId)
	position := t.getVisibleItemIndex(parentId, menuItemId)
	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, err := pInsertMenuItem.Call(
		menu,
		uintptr(position),
		1,
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return err
	}

	return nil
}

// Remove a menu item.
func (t *winTray) removeMenuItem(menuItemId, parentId uint32) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const MF_BYCOMMAND = 0x00000000
	const ERROR_SUCCESS syscall.Errno = 0

	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, err := pDeleteMenu.Call(
		menu,
		uintptr(menuItemId),
		MF_BYCOMMAND,
	)
	if res == 0 && err.(syscall.Errno) != ERROR_SUCCESS {
		return err
	}
	t.delFromVisibleItems(parentId, menuItemId)

	return nil
}

// Hide a menu item.
func (t *winTray) hideMenuItem(menuItemId, parentId uint32) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const MF_BYCOMMAND = 0x00000000
	const ERROR_SUCCESS syscall.Errno = 0

	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, err := pRemoveMenu.Call(
		menu,
		uintptr(menuItemId),
		MF_BYCOMMAND,
	)
	if res == 0 && err.(syscall.Errno) != ERROR_SUCCESS {
		return err
	}
	t.delFromVisibleItems(parentId, menuItemId)

	return nil
}

// Show the menu.
func (t *winTray) showMenu() error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const (
		TPM_BOTTOMALIGN = 0x0020
		TPM_LEFTALIGN   = 0x0000
	)
	p := point{}
	res, _, err := pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if res == 0 {
		return err
	}
	pSetForegroundWindow.Call(uintptr(t.window))

	res, _, err = pTrackPopupMenu.Call(
		uintptr(t.menus[0]),
		TPM_BOTTOMALIGN|TPM_LEFTALIGN,
		uintptr(p.X),
		uintptr(p.Y),
		0,
		uintptr(t.window),
		0,
	)
	if res == 0 {
		return err
	}

	return nil
}

// Remove the item ID from the list of visible items.
func (t *winTray) delFromVisibleItems(parent, val uint32) {
	t.muVisibleItems.Lock()
	defer t.muVisibleItems.Unlock()
	visibleItems := t.visibleItems[parent]
	for i, itemval := range visibleItems {
		if val == itemval {
			t.visibleItems[parent] = append(visibleItems[:i], visibleItems[i+1:]...)
			break
		}
	}
}

// Add the item ID to the list of visible items.
func (t *winTray) addToVisibleItems(parent, val uint32) {
	t.muVisibleItems.Lock()
	defer t.muVisibleItems.Unlock()
	if visibleItems, exists := t.visibleItems[parent]; !exists {
		t.visibleItems[parent] = []uint32{val}
	} else {
		newvisible := append(visibleItems, val)
		sort.Slice(newvisible, func(i, j int) bool { return newvisible[i] < newvisible[j] })
		t.visibleItems[parent] = newvisible
	}
}

// Get the index of the item ID in the list of visible items.
func (t *winTray) getVisibleItemIndex(parent, val uint32) int {
	t.muVisibleItems.RLock()
	defer t.muVisibleItems.RUnlock()
	for i, itemval := range t.visibleItems[parent] {
		if val == itemval {
			return i
		}
	}
	return -1
}

// Load an image from file to be shown in tray or menu item.
// LoadImage: https://msdn.microsoft.com/en-us/library/windows/desktop/ms648045(v=vs.85).aspx
func (t *winTray) loadIconFrom(src string) (windows.Handle, error) {
	if !wt.isReady() {
		return 0, ErrTrayNotReadyYet
	}

	const IMAGE_ICON = 1               // Loads an icon
	const LR_LOADFROMFILE = 0x00000010 // Loads the stand-alone image from the file
	const LR_DEFAULTSIZE = 0x00000040  // Loads default-size icon for windows(SM_CXICON x SM_CYICON) if cx, cy are set to zero

	// Save and reuse handles of loaded images
	t.muLoadedImages.RLock()
	h, ok := t.loadedImages[src]
	t.muLoadedImages.RUnlock()
	if !ok {
		srcPtr, err := windows.UTF16PtrFromString(src)
		if err != nil {
			return 0, err
		}
		res, _, err := pLoadImage.Call(
			0,
			uintptr(unsafe.Pointer(srcPtr)),
			IMAGE_ICON,
			0,
			0,
			LR_LOADFROMFILE|LR_DEFAULTSIZE,
		)
		if res == 0 {
			return 0, err
		}
		h = windows.Handle(res)
		t.muLoadedImages.Lock()
		t.loadedImages[src] = h
		t.muLoadedImages.Unlock()
	}
	return h, nil
}

// Convert an icon handle to a bitmap handle.
func iconToBitmap(hIcon windows.Handle) (windows.Handle, error) {
	const SM_CXSMICON = 49
	const SM_CYSMICON = 50
	const DI_NORMAL = 0x3
	hDC, _, err := pGetDC.Call(uintptr(0))
	if hDC == 0 {
		return 0, err
	}
	defer pReleaseDC.Call(uintptr(0), hDC)
	hMemDC, _, err := pCreateCompatibleDC.Call(hDC)
	if hMemDC == 0 {
		return 0, err
	}
	defer pDeleteDC.Call(hMemDC)
	cx, _, _ := pGetSystemMetrics.Call(SM_CXSMICON)
	cy, _, _ := pGetSystemMetrics.Call(SM_CYSMICON)
	hMemBmp, err := create32BitHBitmap(hMemDC, int32(cx), int32(cy))
	hOriginalBmp, _, _ := pSelectObject.Call(hMemDC, hMemBmp)
	defer pSelectObject.Call(hMemDC, hOriginalBmp)
	res, _, err := pDrawIconEx.Call(hMemDC, 0, 0, uintptr(hIcon), cx, cy, 0, uintptr(0), DI_NORMAL)
	if res == 0 {
		return 0, err
	}
	return windows.Handle(hMemBmp), nil
}

// Create a 32-bit HBITMAP (for use in iconToBitmap).
// https://learn.microsoft.com/en-us/windows/win32/api/wingdi/nf-wingdi-createdibsection
func create32BitHBitmap(hDC uintptr, cx, cy int32) (uintptr, error) {
	const BI_RGB uint32 = 0
	const DIB_RGB_COLORS = 0
	bmi := bitmapInfo{
		BmiHeader: bitmapInfoHeader{
			BiPlanes:      1,
			BiCompression: BI_RGB,
			BiWidth:       cx,
			BiHeight:      cy,
			BiBitCount:    32,
		},
	}
	bmi.BmiHeader.BiSize = uint32(unsafe.Sizeof(bmi.BmiHeader))
	var bits uintptr
	hBitmap, _, err := pCreateDIBSection.Call(
		hDC,
		uintptr(unsafe.Pointer(&bmi)),
		DIB_RGB_COLORS,
		uintptr(unsafe.Pointer(&bits)),
		uintptr(0),
		0,
	)
	if hBitmap == 0 {
		return 0, err
	}
	return hBitmap, nil
}

// Run the systray message loop.
func nativeLoop() {
	// MSG struct
	var m = &struct {
		WindowHandle windows.Handle
		Message      uint32
		Wparam       uintptr
		Lparam       uintptr
		Time         uint32
		Pt           point
	}{}
	for {
		ret, _, err := pGetMessage.Call(uintptr(unsafe.Pointer(m)), 0, 0, 0)

		// If the function retrieves a message other than WM_QUIT, the return value is nonzero.
		// If the function retrieves the WM_QUIT message, the return value is zero.
		// If there is an error, the return value is -1
		// https://msdn.microsoft.com/en-us/library/windows/desktop/ms644936(v=vs.85).aspx
		switch int32(ret) {
		case -1:
			log.Printf("systray error: message loop failure: %s\n", err)
			return
		case 0:
			return
		default:
			pTranslateMessage.Call(uintptr(unsafe.Pointer(m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(m)))
		}
	}
}

// Quit the systray message loop.
func quit() {
	const WM_CLOSE = 0x0010

	pPostMessage.Call(
		uintptr(wt.window),
		WM_CLOSE,
		0,
		0,
	)

	wt.muNID.Lock()
	if wt.nid != nil {
		wt.nid.delete()
	}
	wt.muNID.Unlock()
	systrayExitOnce.Do(systrayExit)
}

// Write the icon bytes to a temp file and return the file path.
func iconBytesToFilePath(iconBytes []byte) (string, error) {
	bh := md5.Sum(iconBytes)
	dataHash := hex.EncodeToString(bh[:])
	iconFilePath := filepath.Join(os.TempDir(), "systray_temp_icon_"+dataHash)

	if _, err := os.Stat(iconFilePath); os.IsNotExist(err) {
		if err := os.WriteFile(iconFilePath, iconBytes, 0644); err != nil {
			return "", err
		}
	}
	return iconFilePath, nil
}

// Set the systray icon.
// iconBytes should be the content of .ico image.
func SetIcon(iconBytes []byte) error {
	iconFilePath, err := iconBytesToFilePath(iconBytes)
	if err != nil {
		return fmt.Errorf("failed to write icon data to temp file: %w", err)
	}
	if err := wt.setIcon(iconFilePath); err != nil {
		return fmt.Errorf("failed to set icon: %w", err)
	}
	return nil
}

// Set the systray icon from a file path.
// iconFilePath should be the path to a .ico image.
func SetIconFromFilePath(iconFilePath string) error {
	return wt.setIcon(iconFilePath)
}

// Return the ID of the parent menu item or 0 if it doesn't have a parent.
func (item *MenuItem) parentId() uint32 {
	if item.parent != nil {
		return uint32(item.parent.id)
	}
	return 0
}

// Set the icon of a menu item.
// iconBytes should be the content of .ico image.
func (item *MenuItem) SetIcon(iconBytes []byte) error {
	iconFilePath, err := iconBytesToFilePath(iconBytes)
	if err != nil {
		return fmt.Errorf("failed to get icon file path: %w", err)
	}

	h, err := wt.loadIconFrom(iconFilePath)
	if err != nil {
		return fmt.Errorf("failed to load icon: %w", err)
	}

	h, err = iconToBitmap(h)
	if err != nil {
		return fmt.Errorf("failed to convert icon to bitmap: %w", err)
	}
	wt.muMenuItemIcons.Lock()
	wt.menuItemIcons[uint32(item.id)] = h
	wt.muMenuItemIcons.Unlock()

	err = wt.addOrUpdateMenuItem(uint32(item.id), item.parentId(), item.title, item.disabled, item.checked)
	if err != nil {
		return fmt.Errorf("failed to update menu item: %w", err)
	}
	return nil
}

// Set the icon of a menu item from a file path.
// iconFilePath should be the path to a .ico image.
func (item *MenuItem) SetIconFromFilePath(iconFilePath string) error {
	h, err := wt.loadIconFrom(iconFilePath)
	if err != nil {
		return fmt.Errorf("failed to load icon: %w", err)
	}

	h, err = iconToBitmap(h)
	if err != nil {
		return fmt.Errorf("failed to convert icon to bitmap: %w", err)
	}
	wt.muMenuItemIcons.Lock()
	wt.menuItemIcons[uint32(item.id)] = h
	wt.muMenuItemIcons.Unlock()

	err = wt.addOrUpdateMenuItem(uint32(item.id), item.parentId(), item.title, item.disabled, item.checked)
	if err != nil {
		return fmt.Errorf("failed to update menu item: %w", err)
	}
	return nil
}

// Set the tooltip to display on mouse hover of the tray icon.
func SetTooltip(tooltip string) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}
	const NIF_TIP = 0x00000004
	b, err := windows.UTF16FromString(tooltip)
	if err != nil {
		return err
	}
	wt.muNID.Lock()
	defer wt.muNID.Unlock()
	copy(wt.nid.Tip[:], b[:])
	wt.nid.Flags |= NIF_TIP
	wt.nid.Size = uint32(unsafe.Sizeof(*wt.nid))
	err = wt.nid.modify()
	if err != nil {
		return fmt.Errorf("failed to set tooltip: %w", err)
	}
	return nil
}

// Add or update a menu item.
func addOrUpdateMenuItem(item *MenuItem) {
	err := wt.addOrUpdateMenuItem(uint32(item.id), item.parentId(), item.title, item.disabled, item.checked)
	if err != nil {
		log.Printf("systray error: unable to add or update menu item: %s\n", err)
	}
}

// Add a separator to the menu.
func addSeparator(id uint32, parent uint32) {
	err := wt.addSeparatorMenuItem(id, parent)
	if err != nil {
		log.Printf("systray error: unable to add separator: %s\n", err)
	}
}
