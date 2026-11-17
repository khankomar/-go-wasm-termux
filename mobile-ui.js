import { LinuxVMManager } from './vm-manager.js';

let fontSize = 14;
const term = new window.Terminal({
  cursorBlink: true,
  fontSize: fontSize,
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
  screenReaderMode: true, // Prevents duplicate characters on Android Chrome Gboard/SwiftKey IME
  scrollback: 3000,
  theme: {
    background: '#0d1117',
    foreground: '#e6edf3',
    cursor: '#58a6ff',
    selectionBackground: '#1f6feb66'
  }
});

const fitAddon = new window.FitAddon.FitAddon();
term.loadAddon(fitAddon);
term.loadAddon(new window.WebLinksAddon.WebLinksAddon());

try {
  const webglAddon = new window.WebglAddon.WebglAddon();
  term.loadAddon(webglAddon);
} catch (_) {
  // Fallback to standard canvas renderer if WebGL context is unavailable
}

const container = document.getElementById('terminal-container');
term.open(container);
fitAddon.fit();

const vm = new LinuxVMManager(term);
vm.boot(false);

// 1. Dynamic Viewport Adjustment for Android Soft Keyboard
const appShell = document.getElementById('app-shell');
if (window.visualViewport) {
  window.visualViewport.addEventListener('resize', () => {
    appShell.style.height = `${window.visualViewport.height}px`;
    setTimeout(() => {
      fitAddon.fit();
      term.scrollToBottom();
    }, 35);
  });
}

// 2. Sticky CTRL & ALT Modifier State Machine
let ctrlSticky = false;
let altSticky = false;
const ctrlBtn = document.getElementById('key-ctrl');
const altBtn = document.getElementById('key-alt');

function setCtrlSticky(state) {
  ctrlSticky = state;
  ctrlBtn.classList.toggle('active-sticky', ctrlSticky);
}

function setAltSticky(state) {
  altSticky = state;
  altBtn.classList.toggle('active-sticky', altSticky);
}

// Forward Xterm.js input to Linux ttyS0 serial port
term.onData((data) => {
  if (ctrlSticky && data.length === 1) {
    const code = data.toUpperCase().charCodeAt(0) - 64;
    setCtrlSticky(false);
    if (code > 0 && code < 32) {
      vm.sendSerialText(String.fromCharCode(code));
      return;
    }
  }

  if (altSticky && data.length === 1) {
    setAltSticky(false);
    vm.sendSerialText('\x1b' + data);
    return;
  }

  vm.sendSerialText(data);
});

// 3. Termux Extra Keys Bar Bindings
const KEY_MAP = {
  ESC: '\x1b',
  TAB: '\t',
  UP: '\x1b[A',
  DOWN: '\x1b[B',
  RIGHT: '\x1b[C',
  LEFT: '\x1b[D',
  HOME: '\x1b[H',
  END: '\x1b[F',
  PGUP: '\x1b[5~',
  PGDN: '\x1b[6~',
  '/': '/',
  '-': '-'
};

document.querySelectorAll('.extra-key').forEach((btn) => {
  btn.addEventListener('click', (e) => {
    e.preventDefault();
    const key = btn.dataset.key;
    if (key === 'CTRL') {
      setCtrlSticky(!ctrlSticky);
    } else if (key === 'ALT') {
      setAltSticky(!altSticky);
    } else if (KEY_MAP[key]) {
      vm.sendSerialText(KEY_MAP[key]);
    }
    term.focus();
  });
});

// 4. Two-Finger Pinch-to-Zoom Gesture on Terminal
let initialPinchDistance = null;
container.addEventListener('touchstart', (e) => {
  if (e.touches.length === 2) {
    initialPinchDistance = Math.hypot(
      e.touches[0].pageX - e.touches[1].pageX,
      e.touches[0].pageY - e.touches[1].pageY
    );
  }
}, { passive: true });

container.addEventListener('touchmove', (e) => {
  if (e.touches.length === 2 && initialPinchDistance) {
    const currentDist = Math.hypot(
      e.touches[0].pageX - e.touches[1].pageX,
      e.touches[0].pageY - e.touches[1].pageY
    );
    const delta = currentDist - initialPinchDistance;
    if (Math.abs(delta) > 25) {
      fontSize = Math.min(24, Math.max(10, fontSize + (delta > 0 ? 1 : -1)));
      term.options.fontSize = fontSize;
      fitAddon.fit();
      initialPinchDistance = currentDist;
    }
  }
}, { passive: true });

container.addEventListener('touchend', () => {
  initialPinchDistance = null;
});

// 5. Top Bar & Slide-out Drawer Actions
document.getElementById('btn-ctrl-c').onclick = () => {
  vm.sendSerialText('\x03');
  term.focus();
};

document.getElementById('btn-quick-save').onclick = () => {
  vm.saveStateToIndexedDB();
  term.focus();
};

const drawer = document.getElementById('system-drawer');
const snapshotInfo = document.getElementById('snapshot-info');

async function updateSnapshotLabel() {
  const snap = await vm.storage.loadSnapshot();
  if (snap) {
    snapshotInfo.textContent = `${(snap.size / 1048576).toFixed(1)} MB Saved`;
  } else {
    snapshotInfo.textContent = 'No Snapshot';
  }
}

document.getElementById('btn-open-drawer').onclick = () => {
  updateSnapshotLabel();
  drawer.classList.remove('hidden');
};

document.getElementById('btn-close-drawer').onclick = () => {
  drawer.classList.add('hidden');
  term.focus();
};

document.getElementById('btn-save-state').onclick = async () => {
  await vm.saveStateToIndexedDB();
  updateSnapshotLabel();
};

document.getElementById('btn-restore-state').onclick = () => {
  drawer.classList.add('hidden');
  vm.boot(false);
};

document.getElementById('btn-export-state').onclick = () => {
  vm.exportStateFile();
};

document.getElementById('btn-clear-state').onclick = async () => {
  await vm.storage.clearSnapshot();
  updateSnapshotLabel();
  drawer.classList.add('hidden');
  vm.boot(true);
};

document.getElementById('input-upload-9p').onchange = (e) => {
  const file = e.target.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = async () => {
    const uint8 = new Uint8Array(reader.result);
    await vm.injectFileToRoot(file.name, uint8);
    drawer.classList.add('hidden');
  };
  reader.readAsArrayBuffer(file);
};

document.getElementById('btn-extract-9p').onclick = () => {
  const path = document.getElementById('input-extract-path').value.trim();
  if (path) vm.extractFileFromVM(path);
};

document.getElementById('btn-apply-relay').onclick = () => {
  const url = document.getElementById('input-relay-url').value.trim();
  if (url) {
    localStorage.setItem('wasm_termux_relay', url);
    vm.relayUrl = url;
    drawer.classList.add('hidden');
    vm.boot(true);
  }
};

document.getElementById('btn-send-paste').onclick = () => {
  const box = document.getElementById('input-paste-box');
  if (box.value) {
    vm.sendSerialText(box.value + '\n');
    box.value = '';
    drawer.classList.add('hidden');
    term.focus();
  }
};
