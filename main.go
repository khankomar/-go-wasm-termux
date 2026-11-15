//go:build js && wasm

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"golang.org/x/crypto/ssh"
)

// ==================== سیستم‌فایل مجازی (VFS) ====================

type FileNode struct {
	Name     string               `json:"name"`
	IsDir    bool                 `json:"is_dir"`
	Content  string               `json:"content"`
	ModTime  string               `json:"mod_time"`
	Children map[string]*FileNode `json:"children,omitempty"`
}

// وضعیت نشست‌های زنده شبکه (Interactive Sessions)
type SessionMode int

const (
	ModeLocalShell SessionMode = iota
	ModeWSCat
	ModeSSHPromptPass
	ModeSSHShell
)

type Shell struct {
	Root    *FileNode         `json:"root"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	History []string          `json:"history"`
	termOut js.Value

	// مدیریت نشست‌های زنده (wscat / ssh)
	mu            sync.Mutex
	mode          SessionMode
	activeWS      js.Value
	sshStdin      io.WriteCloser
	sshSession    *ssh.Session
	sshClient     *ssh.Client
	sshPendingCfg struct {
		user   string
		wsURL  string
		cols   int
		rows   int
	}
}

var sh *Shell

func main() {
	sh = &Shell{
		Cwd:  "/root",
		mode: ModeLocalShell,
		Env: map[string]string{
			"USER":  "root",
			"HOME":  "/root",
			"SHELL": "/bin/gosh",
			"PATH":  "/bin:/usr/bin",
			"TERM":  "xterm-256color",
		},
		History: []string{},
	}

	sh.initVFS()
	sh.loadStateFromStorage()

	// توابع قابل فراخوانی از جاوااسکریپت
	js.Global().Set("goShellInit", js.FuncOf(sh.jsInit))
	js.Global().Set("goShellExec", js.FuncOf(sh.jsExec))
	js.Global().Set("goShellGetPrompt", js.FuncOf(sh.jsGetPrompt))
	js.Global().Set("goShellGetMode", js.FuncOf(sh.jsGetMode))
	js.Global().Set("goShellSendRaw", js.FuncOf(sh.jsSendRaw))
	js.Global().Set("goShellResizePTY", js.FuncOf(sh.jsResizePTY))
	js.Global().Set("goShellUploadFile", js.FuncOf(sh.jsUploadFile))
	js.Global().Set("goShellReadFile", js.FuncOf(sh.jsReadFile))
	js.Global().Set("goShellListCwd", js.FuncOf(sh.jsListCwd))

	select {}
}

func (s *Shell) initVFS() {
	now := time.Now().Format("Jan 02 15:04")
	s.Root = &FileNode{
		Name: "/", IsDir: true, ModTime: now,
		Children: map[string]*FileNode{
			"root": {
				Name: "root", IsDir: true, ModTime: now,
				Children: map[string]*FileNode{
					"welcome.txt": {
						Name:    "welcome.txt",
						IsDir:   false,
						ModTime: now,
						Content: "Go-WASM Termux (Network & Tunnel Edition)\n" +
							"- Test WebSocket: wscat wss://echo.websocket.org\n" +
							"- Connect SSH over WS: ssh root@wss://your-tunnel.domain.com/ssh\n" +
							"- Check latency: httping https://cloudflare.com\n",
					},
				},
			},
			"bin": {Name: "bin", IsDir: true, ModTime: now, Children: map[string]*FileNode{}},
			"tmp": {Name: "tmp", IsDir: true, ModTime: now, Children: map[string]*FileNode{}},
			"etc": {Name: "etc", IsDir: true, ModTime: now, Children: map[string]*FileNode{}},
		},
	}
}

func (s *Shell) print(text string) {
	if !s.termOut.IsUndefined() && !s.termOut.IsNull() {
		formatted := strings.ReplaceAll(text, "\n", "\r\n")
		s.termOut.Invoke(formatted)
	}
}

func (s *Shell) printRaw(text string) {
	if !s.termOut.IsUndefined() && !s.termOut.IsNull() {
		s.termOut.Invoke(text)
	}
}

func (s *Shell) println(text string) {
	s.print(text + "\n")
}

func (s *Shell) saveStateToStorage() {
	data, err := json.Marshal(s.Root)
	if err == nil {
		js.Global().Get("localStorage").Call("setItem", "gowasm_vfs_v1", string(data))
	}
}

func (s *Shell) loadStateFromStorage() {
	val := js.Global().Get("localStorage").Call("getItem", "gowasm_vfs_v1")
	if !val.IsNull() && !val.IsUndefined() && val.String() != "" {
		var loadedRoot FileNode
		if err := json.Unmarshal([]byte(val.String()), &loadedRoot); err == nil {
			s.Root = &loadedRoot
		}
	}
}

func (s *Shell) resolvePath(p string) string {
	if strings.HasPrefix(p, "~") {
		p = s.Env["HOME"] + strings.TrimPrefix(p, "~")
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(s.Cwd, p)
	}
	return path.Clean(p)
}

func (s *Shell) getNode(absPath string) (*FileNode, *FileNode, string) {
	absPath = path.Clean(absPath)
	if absPath == "/" {
		return s.Root, nil, "/"
	}
	parts := strings.Split(strings.TrimPrefix(absPath, "/"), "/")
	curr := s.Root
	var parent *FileNode
	var lastName string

	for i, part := range parts {
		parent = curr
		lastName = part
		if !curr.IsDir || curr.Children == nil {
			return nil, nil, ""
		}
		next, exists := curr.Children[part]
		if !exists {
			if i == len(parts)-1 {
				return nil, parent, lastName
			}
			return nil, nil, ""
		}
		curr = next
	}
	return curr, parent, lastName
}

// ==================== رابط‌های جاوااسکریپت ====================

func (s *Shell) jsInit(this js.Value, args []js.Value) any {
	if len(args) > 0 {
		s.termOut = args[0]
	}
	s.println("\x1b[1;32mGo-WASM Termux (SSH + WebSocket Tunnel Ready)\x1b[0m")
	s.println("Type \x1b[1;36mhelp\x1b[0m to list commands. Try \x1b[1;33mwscat wss://echo.websocket.org\x1b[0m\n")
	return nil
}

func (s *Shell) jsGetPrompt(this js.Value, args []js.Value) any {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mode == ModeWSCat {
		return "\x1b[1;35mwscat>\x1b[0m "
	}
	if s.mode == ModeSSHPromptPass {
		return fmt.Sprintf("\x1b[1;33m%s's password:\x1b[0m ", s.sshPendingCfg.user)
	}
	if s.mode == ModeSSHShell {
		return ""
	}

	displayDir := s.Cwd
	if strings.HasPrefix(displayDir, s.Env["HOME"]) {
		displayDir = "~" + strings.TrimPrefix(displayDir, s.Env["HOME"])
	}
	return fmt.Sprintf("\x1b[1;32m%s@android\x1b[0m:\x1b[1;34m%s\x1b[0m# ", s.Env["USER"], displayDir)
}

// برگرداندن وضعیت فعلی شل به فرانت‌اند:
// 0 = Normal Shell, 1 = wscat line mode, 2 = Password Hidden Input, 3 = Raw SSH PTY stream
func (s *Shell) jsGetMode(this js.Value, args []js.Value) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.mode)
}

// ارسال مستقیم کلیدها در حالت SSH PTY یا سیگنال قطع ارتباط (Ctrl+C)
func (s *Shell) jsSendRaw(this js.Value, args []js.Value) any {
	if len(args) == 0 {
		return nil
	}
	data := args[0].String()

	s.mu.Lock()
	currentMode := s.mode
	s.mu.Unlock()

	// اگر در حالت wscat کاربر Ctrl+C زد، سوکت را ببند
	if currentMode == ModeWSCat && data == "\u0003" {
		if !s.activeWS.IsUndefined() && !s.activeWS.IsNull() {
			s.activeWS.Call("close")
		}
		return nil
	}

	// اگر در حالت SSH Shell هستیم، تمام کلیدها مستقیماً به استریم رمزنگاری‌شده SSH ارسال می‌شوند
	if currentMode == ModeSSHShell && s.sshStdin != nil {
		go func() {
			_, _ = s.sshStdin.Write([]byte(data))
		}()
	}
	return nil
}

// تغییر سایز پویا PTY هنگام باز و بسته شدن کیبورد اندروید در نشست SSH
func (s *Shell) jsResizePTY(this js.Value, args []js.Value) any {
	if len(args) < 2 {
		return nil
	}
	cols := args[0].Int()
	rows := args[1].Int()

	s.mu.Lock()
	session := s.sshSession
	mode := s.mode
	s.mu.Unlock()

	if mode == ModeSSHShell && session != nil {
		go func() {
			_ = session.WindowChange(rows, cols)
		}()
	}
	return nil
}

func (s *Shell) jsExec(this js.Value, args []js.Value) any {
	if len(args) == 0 {
		return nil
	}
	rawCmd := args[0].String()
	var doneCallback js.Value
	if len(args) > 1 {
		doneCallback = args[1]
	}

	go func() {
		defer func() {
			if !doneCallback.IsUndefined() && !doneCallback.IsNull() {
				doneCallback.Invoke()
			}
		}()

		s.mu.Lock()
		currentMode := s.mode
		s.mu.Unlock()

		// ۱. اگر در حالت تعاملی wscat هستیم
		if currentMode == ModeWSCat {
			trimmed := strings.TrimSpace(rawCmd)
			if trimmed == ".exit" || trimmed == "exit" {
				if !s.activeWS.IsUndefined() && !s.activeWS.IsNull() {
					s.activeWS.Call("close")
				}
				return
			}
			if !s.activeWS.IsUndefined() && !s.activeWS.IsNull() {
				s.activeWS.Call("send", rawCmd)
			}
			return
		}

		// ۲. اگر منتظر دریافت رمز عبور SSH هستیم
		if currentMode == ModeSSHPromptPass {
			password := rawCmd
			s.connectSSHWithPassword(password)
			return
		}

		// ۳. حالت عادی شل محلی
		rawCmd = strings.TrimSpace(rawCmd)
		if rawCmd == "" {
			return
		}
		s.History = append(s.History, rawCmd)
		s.executeLine(rawCmd)
	}()

	return nil
}

// ==================== پیاده‌سازی wscat (WebSocket Client) ====================

func (s *Shell) startWSCat(targetURL string) {
	if !strings.HasPrefix(targetURL, "ws://") && !strings.HasPrefix(targetURL, "wss://") {
		targetURL = "wss://" + targetURL
	}

	s.println(fmt.Sprintf("\x1b[1;36mConnecting to %s ...\x1b[0m", targetURL))

	wsConstructor := js.Global().Get("WebSocket")
	ws := wsConstructor.New(targetURL)

	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	onOpen := js.FuncOf(func(this js.Value, args []js.Value) any {
		s.mu.Lock()
		s.mode = ModeWSCat
		s.activeWS = ws
		s.mu.Unlock()

		s.println("\x1b[1;32mConnected! (Type messages and press Enter. Type '.exit' or press Ctrl+C to disconnect)\x1b[0m")
		s.print("\x1b[1;35mwscat>\x1b[0m ")
		closeDone()
		return nil
	})

	onMessage := js.FuncOf(func(this js.Value, args []js.Value) any {
		ev := args[0]
		data := ev.Get("data")
		if data.Type() == js.TypeString {
			s.printRaw(fmt.Sprintf("\r\x1b[K\x1b[1;32m< %s\x1b[0m\r\n\x1b[1;35mwscat>\x1b[0m ", data.String()))
		} else {
			s.printRaw("\r\x1b[K\x1b[1;32m< [Binary Frame Received]\x1b[0m\r\n\x1b[1;35mwscat>\x1b[0m ")
		}
		return nil
	})

	onError := js.FuncOf(func(this js.Value, args []js.Value) any {
		s.println("\r\n\x1b[1;31m[WebSocket Error: Connection failed or blocked]\x1b[0m")
		closeDone()
		return nil
	})

	onClose := js.FuncOf(func(this js.Value, args []js.Value) any {
		s.mu.Lock()
		wasWSCat := s.mode == ModeWSCat
		s.mode = ModeLocalShell
		s.activeWS = js.Undefined()
		s.mu.Unlock()

		if wasWSCat {
			s.println("\r\n\x1b[1;33m[WebSocket Disconnected]\x1b[0m")
			s.printRaw(s.jsGetPrompt(js.Null(), nil).(string))
		}
		closeDone()
		return nil
	})

	ws.Set("onopen", onOpen)
	ws.Set("onmessage", onMessage)
	ws.Set("onerror", onError)
	ws.Set("onclose", onClose)

	<-done
}

// ==================== پیاده‌سازی آداپتور net.Conn روی WebSocket برای SSH ====================

type wsNetConn struct {
	ws        js.Value
	readCh    chan []byte
	currBuf   []byte
	closed    bool
	closeOnce sync.Once
	mu        sync.Mutex
	funcs     []js.Func
}

func dialWebSocketConn(wsURL string) (net.Conn, error) {
	wsConstructor := js.Global().Get("WebSocket")
	ws := wsConstructor.New(wsURL)
	ws.Set("binaryType", "arraybuffer")

	c := &wsNetConn{
		ws:     ws,
		readCh: make(chan []byte, 256),
	}

	openCh := make(chan error, 1)

	onOpen := js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case openCh <- nil:
		default:
		}
		return nil
	})

	onError := js.FuncOf(func(this js.Value, args []js.Value) any {
		select {
		case openCh <- errors.New("websocket connection failed"):
		default:
		}
		c.Close()
		return nil
	})

	onClose := js.FuncOf(func(this js.Value, args []js.Value) any {
		c.Close()
		return nil
	})

	onMessage := js.FuncOf(func(this js.Value, args []js.Value) any {
		data := args[0].Get("data")
		if data.Type() == js.TypeString {
			c.readCh <- []byte(data.String())
		} else {
			uint8Arr := js.Global().Get("Uint8Array").New(data)
			length := uint8Arr.Get("length").Int()
			buf := make([]byte, length)
			js.CopyBytesToGo(buf, uint8Arr)
			c.readCh <- buf
		}
		return nil
	})

	c.funcs = []js.Func{onOpen, onError, onClose, onMessage}
	ws.Set("onopen", onOpen)
	ws.Set("onerror", onError)
	ws.Set("onclose", onClose)
	ws.Set("onmessage", onMessage)

	select {
	case err := <-openCh:
		if err != nil {
			return nil, err
		}
		return c, nil
	case <-time.After(12 * time.Second):
		c.Close()
		return nil, errors.New("connection timeout (12s)")
	}
}

func (c *wsNetConn) Read(b []byte) (int, error) {
	if len(c.currBuf) > 0 {
		n := copy(b, c.currBuf)
		c.currBuf = c.currBuf[n:]
		return n, nil
	}
	chunk, ok := <-c.readCh
	if !ok {
		return 0, io.EOF
	}
	n := copy(b, chunk)
	if n < len(chunk) {
		c.currBuf = chunk[n:]
	}
	return n, nil
}

func (c *wsNetConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.mu.Unlock()

	uint8Arr := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(uint8Arr, b)
	c.ws.Call("send", uint8Arr)
	return len(b), nil
}

func (c *wsNetConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.readCh)
		if !c.ws.IsUndefined() && !c.ws.IsNull() {
			c.ws.Call("close")
		}
		for _, fn := range c.funcs {
			fn.Release()
		}
	})
	return nil
}

type dummyAddr struct{ s string }

func (d dummyAddr) Network() string { return "ws" }
func (d dummyAddr) String() string  { return d.s }

func (c *wsNetConn) LocalAddr() net.Addr                { return dummyAddr{"android-wasm"} }
func (c *wsNetConn) RemoteAddr() net.Addr               { return dummyAddr{"remote-ssh-ws"} }
func (c *wsNetConn) SetDeadline(t time.Time) error      { return nil }
func (c *wsNetConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *wsNetConn) SetWriteDeadline(t time.Time) error { return nil }

// ==================== پیاده‌سازی کلاینت SSH واقعی ====================

func (s *Shell) initiateSSHCommand(args []string) {
	if len(args) == 0 {
		s.println("\x1b[1;33mUsage:\x1b[0m ssh <user>@<wss://your-ssh-ws-tunnel-url>")
		s.println("\x1b[1;36mNote:\x1b[0m Since browsers block raw TCP port 22, pass a WebSocket-to-TCP tunnel endpoint")
		s.println("      (e.g., Cloudflare Tunnel, websockify, or Cloudflare Worker relay).")
		s.println("      Example: \x1b[1;32mssh root@wss://ssh.mydomain.com\x1b[0m")
		return
	}

	target := args[0]
	user := "root"
	wsURL := target

	if strings.Contains(target, "@") {
		parts := strings.SplitN(target, "@", 2)
		user = parts[0]
		wsURL = parts[1]
	}

	if !strings.HasPrefix(wsURL, "ws://") && !strings.HasPrefix(wsURL, "wss://") {
		wsURL = "wss://" + wsURL
	}

	cols := js.Global().Get("termCols").Int()
	rows := js.Global().Get("termRows").Int()
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	s.mu.Lock()
	s.sshPendingCfg.user = user
	s.sshPendingCfg.wsURL = wsURL
	s.sshPendingCfg.cols = cols
	s.sshPendingCfg.rows = rows
	s.mode = ModeSSHPromptPass
	s.mu.Unlock()
}

func (s *Shell) connectSSHWithPassword(password string) {
	s.mu.Lock()
	cfg := s.sshPendingCfg
	s.mu.Unlock()

	s.println(fmt.Sprintf("\r\n\x1b[1;36mDialing WebSocket tunnel %s ...\x1b[0m", cfg.wsURL))

	rawConn, err := dialWebSocketConn(cfg.wsURL)
	if err != nil {
		s.mu.Lock()
		s.mode = ModeLocalShell
		s.mu.Unlock()
		s.println(fmt.Sprintf("\x1b[1;31mssh: failed to connect to tunnel: %v\x1b[0m", err))
		return
	}

	s.println("\x1b[1;36mWebSocket connected! Performing SSHv2 cryptographic handshake...\x1b[0m")

	sshConfig := &ssh.ClientConfig{
		User: cfg.user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         12 * time.Second,
	}

	c, chans, reqs, err := ssh.NewClientConn(rawConn, cfg.wsURL, sshConfig)
	if err != nil {
		_ = rawConn.Close()
		s.mu.Lock()
		s.mode = ModeLocalShell
		s.mu.Unlock()
		s.println(fmt.Sprintf("\x1b[1;31mssh: handshake/auth failed: %v\x1b[0m", err))
		return
	}

	client := ssh.NewClient(c, chans, reqs)
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		s.mu.Lock()
		s.mode = ModeLocalShell
		s.mu.Unlock()
		s.println(fmt.Sprintf("\x1b[1;31mssh: failed to create session: %v\x1b[0m", err))
		return
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	if err := session.RequestPty("xterm-256color", cfg.rows, cfg.cols, modes); err != nil {
		_ = session.Close()
		_ = client.Close()
		s.mu.Lock()
		s.mode = ModeLocalShell
		s.mu.Unlock()
		s.println(fmt.Sprintf("\x1b[1;31mssh: request for pseudo-terminal failed: %v\x1b[0m", err))
		return
	}

	stdinPipe, _ := session.StdinPipe()
	stdoutPipe, _ := session.StdoutPipe()
	stderrPipe, _ := session.StderrPipe()

	if err := session.Shell(); err != nil {
		_ = session.Close()
		_ = client.Close()
		s.mu.Lock()
		s.mode = ModeLocalShell
		s.mu.Unlock()
		s.println(fmt.Sprintf("\x1b[1;31mssh: failed to start shell: %v\x1b[0m", err))
		return
	}

	s.mu.Lock()
	s.mode = ModeSSHShell
	s.sshStdin = stdinPipe
	s.sshSession = session
	s.sshClient = client
	s.mu.Unlock()

	// استریم کردن خروجی سرور SSH به xterm.js
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				s.printRaw(string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				s.printRaw(string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	// منتظر ماندن تا بسته شدن نشست SSH (مثلاً وقتی کاربر داخل سرور تایپ می‌کند exit)
	_ = session.Wait()
	_ = session.Close()
	_ = client.Close()

	s.mu.Lock()
	s.mode = ModeLocalShell
	s.sshStdin = nil
	s.sshSession = nil
	s.sshClient = nil
	s.mu.Unlock()

	s.println("\r\n\x1b[1;33m[Connection to SSH server closed]\x1b[0m")
}

// ==================== پردازش خط فرمان و دستورات محلی ====================

func (s *Shell) executeLine(line string) {
	for k, v := range s.Env {
		line = strings.ReplaceAll(line, "$"+k, v)
	}

	var redirectFile string
	appendMode := false

	if strings.Contains(line, ">>") {
		parts := strings.SplitN(line, ">>", 2)
		line = strings.TrimSpace(parts[0])
		redirectFile = strings.TrimSpace(parts[1])
		appendMode = true
	} else if strings.Contains(line, ">") {
		parts := strings.SplitN(line, ">", 2)
		line = strings.TrimSpace(parts[0])
		redirectFile = strings.TrimSpace(parts[1])
	}

	tokens := parseTokens(line)
	if len(tokens) == 0 {
		return
	}

	cmd := tokens[0]
	cmdArgs := tokens[1:]

	output, isAsync := s.runCommand(cmd, cmdArgs)
	if isAsync {
		return
	}

	if redirectFile != "" {
		targetPath := s.resolvePath(redirectFile)
		node, parent, name := s.getNode(targetPath)
		if parent == nil || !parent.IsDir {
			s.println("\x1b[1;31msh: cannot create file: Invalid path\x1b[0m")
			return
		}
		now := time.Now().Format("Jan 02 15:04")
		if node == nil {
			parent.Children[name] = &FileNode{
				Name: name, IsDir: false, Content: output, ModTime: now,
			}
		} else if !node.IsDir {
			if appendMode {
				node.Content += output
			} else {
				node.Content = output
			}
			node.ModTime = now
		}
		s.saveStateToStorage()
	} else if output != "" {
		s.print(output)
	}
}

func (s *Shell) runCommand(cmd string, args []string) (string, bool) {
	now := time.Now().Format("Jan 02 15:04")

	switch cmd {
	case "help":
		return "\x1b[1;33mGo-WASM Termux Commands:\x1b[0m\n" +
			"  \x1b[1;32mwscat <wss://url>\x1b[0m         Interactive WebSocket client for tunnels & servers\n" +
			"  \x1b[1;32mssh user@wss://host\x1b[0m       Full encrypted SSHv2 client over WebSocket tunnel\n" +
			"  \x1b[1;32mhttping <url>\x1b[0m             Measure HTTP/Tunnel RTT latency over mobile network\n" +
			"  \x1b[1;36mcurl [-o file] <url>\x1b[0m      Fetch HTTP/HTTPS endpoint via Android connection\n" +
			"  \x1b[1;36mls, cd, pwd, cat, mkdir, rm, touch, write, cp, mv, echo, grep\x1b[0m\n" +
			"  \x1b[1;36menv, export, history, uname, whoami, date, clear, resetfs\x1b[0m\n", false

	case "wscat":
		if len(args) == 0 {
			return "Usage: wscat <ws:// or wss:// url>\nExample: wscat wss://echo.websocket.org\n", false
		}
		s.startWSCat(args[0])
		return "", true

	case "ssh":
		s.initiateSSHCommand(args)
		return "", true

	case "httping":
		if len(args) == 0 {
			return "Usage: httping <https://host>\n", false
		}
		s.execHttpPing(args[0])
		return "", true

	case "clear":
		s.print("\x1b[2J\x1b[H")
		return "", false

	case "pwd":
		return s.Cwd + "\n", false

	case "whoami":
		return s.Env["USER"] + "\n", false

	case "uname":
		return "Linux android-wasm 6.6.0-gowasm #1 SMP PREEMPT WebAssembly wasm32 GNU/Linux\n", false

	case "date":
		return time.Now().Format(time.RFC1123) + "\n", false

	case "echo":
		return strings.Join(args, " ") + "\n", false

	case "env":
		var out strings.Builder
		for k, v := range s.Env {
			out.WriteString(fmt.Sprintf("%s=%s\n", k, v))
		}
		return out.String(), false

	case "export":
		for _, arg := range args {
			if kv := strings.SplitN(arg, "=", 2); len(kv) == 2 {
				s.Env[kv[0]] = kv[1]
			}
		}
		return "", false

	case "history":
		var out strings.Builder
		for i, h := range s.History {
			out.WriteString(fmt.Sprintf(" %4d  %s\n", i+1, h))
		}
		return out.String(), false

	case "cd":
		target := s.Env["HOME"]
		if len(args) > 0 {
			target = args[0]
		}
		abs := s.resolvePath(target)
		node, _, _ := s.getNode(abs)
		if node == nil || !node.IsDir {
			return fmt.Sprintf("cd: %s: No such directory\n", target), false
		}
		s.Cwd = abs
		return "", false

	case "ls":
		target := s.Cwd
		longFormat := false
		for _, a := range args {
			if strings.HasPrefix(a, "-") && strings.Contains(a, "l") {
				longFormat = true
			} else if !strings.HasPrefix(a, "-") {
				target = a
			}
		}
		node, _, _ := s.getNode(s.resolvePath(target))
		if node == nil {
			return fmt.Sprintf("ls: cannot access '%s'\n", target), false
		}
		if !node.IsDir {
			return node.Name + "\n", false
		}
		names := make([]string, 0, len(node.Children))
		for k := range node.Children {
			names = append(names, k)
		}
		sort.Strings(names)
		var out strings.Builder
		for _, name := range names {
			child := node.Children[name]
			if longFormat {
				perm := "-rw-r--r--"
				color := "\x1b[0m"
				if child.IsDir {
					perm = "drwxr-xr-x"
					color = "\x1b[1;34m"
				}
				out.WriteString(fmt.Sprintf("%s 1 root root %6d %s %s%s\x1b[0m\n",
					perm, len(child.Content), child.ModTime, color, child.Name))
			} else {
				if child.IsDir {
					out.WriteString(fmt.Sprintf("\x1b[1;34m%s/\x1b[0m  ", child.Name))
				} else {
					out.WriteString(fmt.Sprintf("%s  ", child.Name))
				}
			}
		}
		if !longFormat && len(names) > 0 {
			out.WriteString("\n")
		}
		return out.String(), false

	case "mkdir":
		for _, arg := range args {
			abs := s.resolvePath(arg)
			node, parent, name := s.getNode(abs)
			if node == nil && parent != nil && parent.IsDir {
				parent.Children[name] = &FileNode{
					Name: name, IsDir: true, ModTime: now, Children: map[string]*FileNode{},
				}
			}
		}
		s.saveStateToStorage()
		return "", false

	case "touch":
		for _, arg := range args {
			abs := s.resolvePath(arg)
			node, parent, name := s.getNode(abs)
			if node != nil {
				node.ModTime = now
			} else if parent != nil && parent.IsDir {
				parent.Children[name] = &FileNode{Name: name, IsDir: false, ModTime: now}
			}
		}
		s.saveStateToStorage()
		return "", false

	case "write":
		if len(args) < 2 {
			return "Usage: write <filename> <text...>\n", false
		}
		abs := s.resolvePath(args[0])
		_, parent, name := s.getNode(abs)
		if parent != nil && parent.IsDir {
			parent.Children[name] = &FileNode{
				Name: name, IsDir: false, Content: strings.Join(args[1:], " ") + "\n", ModTime: now,
			}
			s.saveStateToStorage()
		}
		return "", false

	case "cat":
		var out strings.Builder
		for _, arg := range args {
			node, _, _ := s.getNode(s.resolvePath(arg))
			if node != nil && !node.IsDir {
				out.WriteString(node.Content)
				if !strings.HasSuffix(node.Content, "\n") {
					out.WriteString("\n")
				}
			} else {
				out.WriteString(fmt.Sprintf("cat: %s: Invalid file\n", arg))
			}
		}
		return out.String(), false

	case "rm":
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			abs := s.resolvePath(arg)
			if abs != "/" && abs != "/root" {
				_, parent, name := s.getNode(abs)
				if parent != nil {
					delete(parent.Children, name)
				}
			}
		}
		s.saveStateToStorage()
		return "", false

	case "resetfs":
		s.initVFS()
		s.saveStateToStorage()
		return "\x1b[1;32mFilesystem reset.\x1b[0m\n", false

	case "curl":
		if len(args) == 0 {
			return "Usage: curl [-o file] <url>\n", false
		}
		var outFile, targetURL string
		for i := 0; i < len(args); i++ {
			if args[i] == "-o" && i+1 < len(args) {
				outFile = args[i+1]
				i++
			} else if !strings.HasPrefix(args[i], "-") {
				targetURL = args[i]
			}
		}
		if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
			targetURL = "https://" + targetURL
		}
		s.execCurl(targetURL, outFile)
		return "", true

	default:
		return fmt.Sprintf("\x1b[1;31mgosh: %s: command not found\x1b[0m\n", cmd), false
	}
}

func (s *Shell) execHttpPing(targetURL string) {
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://" + targetURL
	}
	s.println(fmt.Sprintf("HTTPING %s (3 probes via Android Fetch):", targetURL))
	for i := 1; i <= 3; i++ {
		start := time.Now()
		done := make(chan error, 1)

		opts := js.Global().Get("Object").New()
		opts.Set("mode", "no-cors")
		opts.Set("cache", "no-store")

		promise := js.Global().Call("fetch", targetURL, opts)
		thenFn := js.FuncOf(func(this js.Value, args []js.Value) any {
			done <- nil
			return nil
		})
		catchFn := js.FuncOf(func(this js.Value, args []js.Value) any {
			done <- errors.New(args[0].Get("message").String())
			return nil
		})

		promise.Call("then", thenFn).Call("catch", catchFn)
		err := <-done
		thenFn.Release()
		catchFn.Release()

		rtt := time.Since(start).Milliseconds()
		if err != nil {
			s.println(fmt.Sprintf("  seq=%d \x1b[1;31mtimeout/unreachable (%v)\x1b[0m", i, err))
		} else {
			s.println(fmt.Sprintf("  seq=%d \x1b[1;32mconnected\x1b[0m time=\x1b[1;36m%d ms\x1b[0m", i, rtt))
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func (s *Shell) execCurl(url string, outFile string) {
	done := make(chan string, 1)
	promise := js.Global().Call("fetch", url)

	thenText := js.FuncOf(func(this js.Value, args []js.Value) any {
		done <- args[0].String()
		return nil
	})
	defer thenText.Release()

	thenResp := js.FuncOf(func(this js.Value, args []js.Value) any {
		args[0].Call("text").Call("then", thenText)
		return nil
	})
	defer thenResp.Release()

	catchErr := js.FuncOf(func(this js.Value, args []js.Value) any {
		done <- fmt.Sprintf("\x1b[1;31mcurl error: %s\x1b[0m\n", args[0].Get("message").String())
		return nil
	})
	defer catchErr.Release()

	promise.Call("then", thenResp).Call("catch", catchErr)
	body := <-done

	if outFile != "" {
		abs := s.resolvePath(outFile)
		_, parent, name := s.getNode(abs)
		if parent != nil && parent.IsDir {
			parent.Children[name] = &FileNode{
				Name: name, IsDir: false, Content: body, ModTime: time.Now().Format("Jan 02 15:04"),
			}
			s.saveStateToStorage()
			s.println(fmt.Sprintf("\x1b[1;32mSaved %d bytes to %s\x1b[0m", len(body), outFile))
		}
	} else {
		s.print(body)
		if !strings.HasSuffix(body, "\n") {
			s.println("")
		}
	}
}

func (s *Shell) jsUploadFile(this js.Value, args []js.Value) any {
	if len(args) < 2 {
		return false
	}
	_, parent, name := s.getNode(path.Join(s.Cwd, args[0].String()))
	if parent != nil && parent.IsDir {
		parent.Children[name] = &FileNode{
			Name: name, IsDir: false, Content: args[1].String(), ModTime: time.Now().Format("Jan 02 15:04"),
		}
		s.saveStateToStorage()
		return true
	}
	return false
}

func (s *Shell) jsReadFile(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.Null()
	}
	node, _, _ := s.getNode(s.resolvePath(args[0].String()))
	if node == nil || node.IsDir {
		return js.Null()
	}
	return node.Content
}

func (s *Shell) jsListCwd(this js.Value, args []js.Value) any {
	node, _, _ := s.getNode(s.Cwd)
	if node == nil || !node.IsDir {
		return "[]"
	}
	type item struct {
		Name  string `json:"name"`
		IsDir bool   `json:"isDir"`
		Size  int    `json:"size"`
	}
	var list []item
	for _, child := range node.Children {
		list = append(list, item{Name: child.Name, IsDir: child.IsDir, Size: len(child.Content)})
	}
	data, _ := json.Marshal(list)
	return string(data)
}

func parseTokens(input string) []string {
	var args []string
	var current strings.Builder
	inQuote := false
	var quoteChar rune
	for _, r := range input {
		switch {
		case inQuote:
			if r == quoteChar {
				inQuote = false
			} else {
				current.WriteRune(r)
			}
		case r == '"' || r == '\'':
			inQuote = true
			quoteChar = r
		case r == ' ' || r == '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}
