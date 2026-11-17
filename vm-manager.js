import { VMStorage } from './storage.js';

export class LinuxVMManager {
  constructor(term) {
    this.term = term;
    this.storage = new VMStorage();
    this.emulator = null;
    this.isBooted = false;
    this.relayUrl = localStorage.getItem('wasm_termux_relay') || 'wss://relay.widgetry.org/';
  }

  updateStatus(text, color = 'amber') {
    const badge = document.getElementById('vm-status-badge');
    const dot = document.getElementById('vm-status-dot');
    const label = document.getElementById('vm-status-text');
    if (!badge || !dot || !label) return;

    label.textContent = text;
    if (color === 'emerald') {
      badge.className = 'flex items-center gap-1.5 text-emerald-400 font-medium';
      dot.className = 'w-2 h-2 rounded-full bg-emerald-400';
    } else if (color === 'blue') {
      badge.className = 'flex items-center gap-1.5 text-blue-400 font-medium';
      dot.className = 'w-2 h-2 rounded-full bg-blue-400 animate-pulse';
    } else {
      badge.className = 'flex items-center gap-1.5 text-amber-400 font-medium';
      dot.className = 'w-2 h-2 rounded-full bg-amber-400 animate-pulse';
    }
  }

  async boot(forceColdBoot = false) {
    if (this.emulator) {
      this.emulator.destroy();
      this.emulator = null;
    }

    this.term.writeln('\x1b[1;36m[WASM-Termux] Initializing x86-to-WASM Virtualization Engine...\x1b[0m');
    const savedState = !forceColdBoot ? await this.storage.loadSnapshot() : null;

    const v86Config = {
      wasm_path: 'https://copy.sh/v86/build/v86.wasm',
      memory_size: 256 * 1024 * 1024, // 256 MB RAM
      vga_memory_size: 2 * 1024 * 1024,
      screen_container: document.getElementById('v86-screen-hidden'),
      bios: { url: 'https://copy.sh/v86/bios/seabios.bin' },
      vga_bios: { url: 'https://copy.sh/v86/bios/vgabios.bin' },
      bzimage: { url: 'https://copy.sh/v86/images/buildroot-bzimage68.bin' },
      cmdline: 'rw init=/sbin/init root=/dev/sda console=ttyS0 mitigations=off',
      network_relay_url: this.relayUrl,
      autostart: true,
      disable_keyboard: true,
      disable_mouse: true
    };

    this.emulator = new window.V86(v86Config);

    // Wire Linux Serial Console (ttyS0) output directly to xterm.js
    this.emulator.add_listener('serial0-output-byte', (byte) => {
      const char = String.fromCharCode(byte);
      this.term.write(char);
      if (!this.isBooted) {
        this.isBooted = true;
        this.updateStatus('Linux Active', 'emerald');
      }
    });

    this.emulator.add_listener('emulator-ready', async () => {
      if (savedState && savedState.buffer) {
        this.updateStatus('Restoring Snapshot...', 'blue');
        this.term.writeln(`\x1b[1;32m[WASM-Termux] Restoring saved RAM snapshot (${(savedState.size / 1048576).toFixed(1)} MB)...\x1b[0m`);
        try {
          await this.emulator.restore_state(savedState.buffer);
          this.isBooted = true;
          this.updateStatus('Linux Active (Restored)', 'emerald');
          this.sendSerialText('\n');
        } catch (err) {
          this.term.writeln(`\x1b[1;31m[Snapshot Restore Error]: ${err.message}. Proceeding with cold boot.\x1b[0m`);
        }
      } else {
        this.term.writeln('\x1b[1;33m[WASM-Termux] Booting Linux kernel on ttyS0 (takes ~5 seconds)...\x1b[0m\r\n');
        setTimeout(() => {
          // Initialize network interface and prompt automatically after boot
          this.sendSerialText('export TERM=xterm-256color\nclear\nuname -a\n');
        }, 5500);
      }
    });
  }

  sendSerialText(str) {
    if (!this.emulator) return;
    this.emulator.serial0_send(str);
  }

  async saveStateToIndexedDB() {
    if (!this.emulator) return;
    this.updateStatus('Saving State...', 'blue');
    const buffer = await this.emulator.save_state();
    const record = await this.storage.saveSnapshot(buffer);
    this.updateStatus('State Saved!', 'emerald');
    this.term.writeln(`\r\n\x1b[1;32m[Snapshot Saved to IndexedDB: ${(record.size / 1048576).toFixed(2)} MB]\x1b[0m`);
    return record;
  }

  async exportStateFile() {
    if (!this.emulator) return;
    const buffer = await this.emulator.save_state();
    const blob = new Blob([buffer], { type: 'application/octet-stream' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `wasm-termux-snapshot-${Date.now()}.bin`;
    a.click();
  }

  async injectFileToRoot(fileName, uint8Array) {
    if (!this.emulator) return;
    const targetPath = `/root/${fileName}`;
    await this.emulator.create_file(targetPath, uint8Array);
    this.term.writeln(`\r\n\x1b[1;32m[9p Bridge] Injected ${fileName} (${uint8Array.byteLength} bytes) -> ${targetPath}\x1b[0m`);
    this.sendSerialText('\n');
  }

  async extractFileFromVM(virtualPath) {
    if (!this.emulator) return;
    try {
      const uint8Array = await this.emulator.read_file(virtualPath);
      const blob = new Blob([uint8Array], { type: 'application/octet-stream' });
      const fileName = virtualPath.split('/').pop() || 'downloaded.bin';
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = fileName;
      a.click();
    } catch (err) {
      alert(`Could not read ${virtualPath} from 9p filesystem: ${err.message}`);
    }
  }
}
